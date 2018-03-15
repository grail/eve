// Copyright (c) 2018 Zededa, Inc.
// All rights reserved.

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	google_protobuf "github.com/golang/protobuf/ptypes/timestamp"
	"github.com/satori/go.uuid"
	"github.com/zededa/api/zmet"
	"github.com/zededa/go-provision/types"
	"github.com/zededa/go-provision/watch"
	"github.com/zededa/go-provision/zedcloud"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	defaultLogdirname = "/var/log"
	identityDirname   = "/config"
	serverFilename    = identityDirname + "/server"
	uuidFileName      = identityDirname + "/uuid"
	DNSDirname        = "/var/run/zedrouter/DeviceNetworkStatus"
)

var devUUID uuid.UUID
var deviceNetworkStatus types.DeviceNetworkStatus
var debug bool
var serverName string
var logsApi string = "api/v1/edgedevice/logs"
var logsUrl string
var zedcloudCtx zedcloud.ZedCloudContext
var logMaxSize = 100

// Key is ifname string
var logs map[string]zedcloudLogs

// global stuff
type logDirModifyHandler func(ctx *loggerContext, logFileName string, source string)
type logDirDeleteHandler func(ctx *loggerContext, logFileName string, source string)

// Set from Makefile
var Version = "No version specified"

// Based on the proto file
type logEntry struct {
	severity  string
	source    string // basename of filename?
	image     string
	iid       string // XXX e.g. PID - where do we get it from?
	content   string // One line
	timestamp *google_protobuf.Timestamp
}

// List of log files we watch
type loggerContext struct {
	logfileReaders []logfileReader
	logChan        chan<- logEntry
}

type logfileReader struct {
	filename string
	source   string
	fileDesc *os.File
	reader   *bufio.Reader
	size     int64 // To detect file truncation
}

// Context for handleDNSModify
type DNSContext struct {
	usableAddressCount int
	triggerGetConfig   bool
}

type zedcloudLogs struct {
	FailureCount uint64
	SuccessCount uint64
	LastFailure  time.Time
	LastSuccess  time.Time
}

func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	versionPtr := flag.Bool("v", false, "Version")
	debugPtr := flag.Bool("d", false, "Debug")
	logdirPtr := flag.String("l", defaultLogdirname, "Log file directory")
	flag.Parse()
	debug = *debugPtr
	if *versionPtr {
		fmt.Printf("%s: %s\n", os.Args[0], Version)
		return
	}
	logDirName := *logdirPtr
	log.Printf("Starting log manager... watching %s\n", logDirName)

	DNSctx := DNSContext{}
	DNSctx.usableAddressCount = types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus)

	networkStatusChanges := make(chan string)
	go watch.WatchStatus(DNSDirname, networkStatusChanges)

	log.Printf("Waiting until we have some uplinks with usable addresses\n")
	for types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus) == 0 {
		select {
		case change := <-networkStatusChanges:
			watch.HandleStatusEvent(change, &DNSctx,
				DNSDirname,
				&types.DeviceNetworkStatus{},
				handleDNSModify, handleDNSDelete,
				nil)
		}
	}

	//Get servername, set logUrl, get device id and initialize zedcloudCtx
	sendCtxInit()

	loggerChan := make(chan logEntry)
	ctx := loggerContext{logChan: loggerChan}
	// Start sender of log events
	// XXX or we run this in main routine and the logDirChanges loop
	// in a go routine??
	go processEvents(loggerChan)

	// XXX The OtherPartition files will not change hence we can just
	// read them and send their lines; no need to watch for changes.
	// Should we read all of them serially?

	logDirChanges := make(chan string)
	go watch.WatchStatus(logDirName, logDirChanges)

	log.Println("called watcher...")
	for {
		select {
		case change := <-logDirChanges:
			HandleLogDirEvent(change, logDirName, &ctx,
				handleLogDirModify, handleLogDirDelete)

		case change := <-networkStatusChanges:
			watch.HandleStatusEvent(change, &DNSctx,
				DNSDirname,
				&types.DeviceNetworkStatus{},
				handleDNSModify, handleDNSDelete,
				nil)
		}
	}
}

func handleDNSModify(ctxArg interface{}, statusFilename string,
	statusArg interface{}) {
	status := statusArg.(*types.DeviceNetworkStatus)
	ctx := ctxArg.(*DNSContext)

	if statusFilename != "global" {
		log.Printf("handleDNSModify: ignoring %s\n", statusFilename)
		return
	}
	log.Printf("handleDNSModify for %s\n", statusFilename)
	deviceNetworkStatus = *status
	// Did we (re-)gain the first usable address?
	// XXX should we also trigger if the count increases?
	newAddrCount := types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus)
	if newAddrCount != 0 && ctx.usableAddressCount == 0 {
		log.Printf("DeviceNetworkStatus from %d to %d addresses\n",
			newAddrCount, ctx.usableAddressCount)
		ctx.triggerGetConfig = true
	}
	ctx.usableAddressCount = newAddrCount
	log.Printf("handleDNSModify done for %s\n", statusFilename)
}

func handleDNSDelete(ctxArg interface{}, statusFilename string) {
	log.Printf("handleDNSDelete for %s\n", statusFilename)
	ctx := ctxArg.(*DNSContext)

	if statusFilename != "global" {
		log.Printf("handleDNSDelete: ignoring %s\n", statusFilename)
		return
	}
	deviceNetworkStatus = types.DeviceNetworkStatus{}
	newAddrCount := types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus)
	ctx.usableAddressCount = newAddrCount
	log.Printf("handleDNSDelete done for %s\n", statusFilename)
}

// This runs as a separate go routine sending out data
func processEvents(logChan <-chan logEntry) {

	reportLogs := new(zmet.LogBundle)
	flushTimer := time.NewTicker(time.Second * 10)
	counter := 0

	for {
		select {
		case event := <-logChan:
			HandleLogEvent(event, reportLogs, counter)
			counter++

			if counter >= logMaxSize {
				sendProtoStrForLogs(reportLogs, iteration)
				counter = 0
				iteration += 1
			}

		case <-flushTimer.C:
			log.Println("Logger Flush at", reportLogs.Timestamp)
			if counter > 0 {
				sendProtoStrForLogs(reportLogs, iteration)
				counter = 0
				iteration += 1
			}
		}
	}
}

var msgIdCounter = 1
var iteration = 0

func HandleLogEvent(event logEntry, reportLogs *zmet.LogBundle, counter int) {
	// Assign a unique msgId for each message
	msgId := msgIdCounter
	msgIdCounter += 1
	// XXX send message over protobuf
	fmt.Printf("Read event from %s time %v id %d: %s\n",
		event.source, event.timestamp, msgId, event.content)
	logDetails := &zmet.LogEntry{}
	logDetails.Content = event.content
	logDetails.Timestamp = event.timestamp
	logDetails.Source = event.source
	logDetails.Msgid = uint64(msgId)
	reportLogs.Log = append(reportLogs.Log, logDetails)
}

func sendProtoStrForLogs(reportLogs *zmet.LogBundle, iteration int) {
	reportLogs.Timestamp = ptypes.TimestampNow()
	reportLogs.DevID = *proto.String(devUUID.String())
	reportLogs.Image = "IMG"
	log.Println("sendProtoStrForLogs called...", iteration)
	log.Println("Log Details: ", reportLogs)
	data, err := proto.Marshal(reportLogs)
	if err != nil {
		log.Fatal("SendInfoProtobufStr proto marshaling error: ", err)
	}
	buf := bytes.NewBuffer(data)
	if buf == nil {
		log.Fatal("SendInfoProtobufStr malloc error:")
	}

	resp, err := zedcloud.SendOnAllIntf(zedcloudCtx, logsUrl,
		buf, iteration)
	if err != nil {
		// Hopefully next timeout will be more successful
		log.Printf("SendMetricsProtobuf failed: %s\n", err)
		return
	}
	reportLogs.Log = []*zmet.LogEntry{}
	resp.Body.Close()
}

func sendCtxInit() {
	//get server name
	bytes, err := ioutil.ReadFile(serverFilename)
	if err != nil {
		log.Fatal(err)
	}
	strTrim := strings.TrimSpace(string(bytes))
	serverName = strings.Split(strTrim, ":")[0]

	//set log url
	logsUrl = serverName + "/" + logsApi

	tlsConfig, err := zedcloud.GetTlsConfig(serverName, nil)
	if err != nil {
		log.Fatal(err)
	}
	zedcloudCtx.DeviceNetworkStatus = &deviceNetworkStatus
	zedcloudCtx.TlsConfig = tlsConfig
	zedcloudCtx.Debug = debug

	b, err := ioutil.ReadFile(uuidFileName)
	if err != nil {
		log.Fatal("ReadFile", err, uuidFileName)
	}
	uuidStr := strings.TrimSpace(string(b))
	devUUID, err = uuid.FromString(uuidStr)
	if err != nil {
		log.Fatal("uuid.FromString", err, string(b))
	}
	fmt.Printf("Read UUID %s\n", devUUID)
}

func HandleLogDirEvent(change string, logDirName string, ctx *loggerContext,
	handleLogDirModifyFunc logDirModifyHandler, handleLogDirDeleteFunc logDirDeleteHandler) {

	operation := string(change[0])
	fileName := string(change[2:])
	if !strings.HasSuffix(fileName, ".log") {
		log.Printf("Ignoring file <%s> operation %s\n",
			fileName, operation)
		return
	}
	logFilePath := logDirName + "/" + fileName
	// Remove .log from name */
	name := strings.Split(fileName, ".log")
	source := name[0]
	if operation == "D" {
		handleLogDirDeleteFunc(ctx, logFilePath, source)
		return
	}
	if operation != "M" {
		log.Fatal("Unknown operation from Watcher: ",
			operation)
	}
	handleLogDirModifyFunc(ctx, logFilePath, source)
}

// If the filename is new we spawn a go routine which will read
func handleLogDirModify(ctx *loggerContext, filename string, source string) {
	for i, r := range ctx.logfileReaders {
		if r.filename == filename {
			readLineToEvent(&ctx.logfileReaders[i], ctx.logChan)
			return
		}
	}
	log.Printf("handleLogDirModify: add %s, source %s\n", filename, source)
	fileDesc, err := os.Open(filename)
	if err != nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	// Start reading from the file with a reader.
	reader := bufio.NewReader(fileDesc)
	if reader == nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	r := logfileReader{filename: filename,
		source:   source,
		fileDesc: fileDesc,
		reader:   reader,
	}
	// read initial entries until EOF
	readLineToEvent(&r, ctx.logChan)
	ctx.logfileReaders = append(ctx.logfileReaders, r)
}

// XXX TBD should we stop the go routine?
func handleLogDirDelete(ctx *loggerContext, filename string, source string) {
}

// Read until EOF or error
func readLineToEvent(r *logfileReader, logChan chan<- logEntry) {
	// Check if shrunk aka truncated
	fi, err := r.fileDesc.Stat()
	if err != nil {
		log.Printf("Stat failed %s\n", err)
		return
	}
	if fi.Size() < r.size {
		log.Printf("File shrunk from %d to %d\n", r.size, fi.Size())
		_, err = r.fileDesc.Seek(0, os.SEEK_SET)
		if err != nil {
			log.Printf("Seek failed %s\n", err)
			return
		}
	}
	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			// XXX do we need to look for truncatation during
			// this loop?
			if debug {
				log.Println(err)
			}
			if err != io.EOF {
				fmt.Printf(" > Failed!: %v\n", err)
			}
			break
		}
		// XXX remove trailing "/n" from line
		// XXX parse timestamp and remove it from line (if present)
		// otherwise leave timestamp unitialized
		parsedDateAndTime, err := parseDateTime(line)
		if err != nil {
			logChan <- logEntry{source: r.source, content: line}
		} else {
			logChan <- logEntry{source: r.source, content: line, timestamp: parsedDateAndTime}
		}

	}
	// Update size
	fi, err = r.fileDesc.Stat()
	if err != nil {
		log.Printf("Stat failed %s\n", err)
		return
	}
	r.size = fi.Size()
}

//parse date and time from agent logs
func parseDateTime(line string) (*google_protobuf.Timestamp, error) {

	var protoDateAndTime *google_protobuf.Timestamp
	re := regexp.MustCompile(`^\d{4}/\d{2}/\d{2}`)
	matched := re.MatchString(line)
	if matched {
		dateAndTime := strings.Split(line, " ")
		re := regexp.MustCompile("/")
		newDateFormat := re.ReplaceAllLiteralString(dateAndTime[0], "-")

		timeFormat := strings.Split(dateAndTime[1], ".")[0]
		newDateAndTime := newDateFormat + "T" + timeFormat
		layout := "2006-01-02T15:04:05"

		///convert newDateAndTime type string to type time.time
		dt, err := time.Parse(layout, newDateAndTime)
		if err != nil {
			log.Println(err)
			return nil, err
		} else {
			//convert dt type time.time to type proto
			protoDateAndTime, err = ptypes.TimestampProto(dt)
			if err != nil {
				log.Println("Error while converting timestamp in proto format: ", err)
				return nil, err
			} else {
				return protoDateAndTime, nil
			}
		}
	} else {
		return nil, errors.New("date and time format not found")
	}
}

// XXX useful to read unchanging files until EOF
// Use for the otherpartition files!
func logReader(logFile string, source string, logChan chan<- logEntry) {
	fileDesc, err := os.Open(logFile)
	if err != nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	// Start reading from the file with a reader.
	reader := bufio.NewReader(fileDesc)
	if reader == nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	r := logfileReader{filename: logFile,
		source:   source,
		fileDesc: fileDesc,
		reader:   reader,
	}
	// read entries until EOF
	readLineToEvent(&r, logChan)
	log.Printf("logReader done for %s\n", logFile)
}