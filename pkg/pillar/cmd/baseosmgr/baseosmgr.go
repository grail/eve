// Copyright (c) 2017-2018 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

// baseosmgr orchestrates base os/certs installation
// interfaces with zedagent for configuration update
// interfaces with downloader for basos image/certs download
// interfaces with verifier for image sha/signature verfication

// baswos handles the following orchestration
//   * base os download config/status <downloader> / <baseos> / <config | status>
//   * base os verifier config/status <verifier>   / <baseos> / <config | status>
//   * certs download config/status   <downloader> / <certs>  / <config | status>
// <base os>
//   <zedagent>   <baseos> <config> --> <baseosmgr>   <baseos> <status>
//				<download>...       --> <downloader>  <baseos> <config>
//   <downloader> <baseos> <config> --> <downloader>  <baseos> <status>
//				<downloaded>...     --> <downloader>  <baseos> <status>
//	 <downloader> <baseos> <status> --> <baseosmgr>   <baseos> <status>
//				<verify>    ...     --> <verifier>    <baseos> <config>
//   <verifier> <baseos> <config>   --> <verifier>    <baseos> <status>
//				<verified>  ...     --> <verifier>    <baseos> <status>
//	 <verifier> <baseos> <status>   --> <baseosmgr>   <baseos> <status>
// <certs>
//   <zedagent>   <certs> <config>  --> <baseosmgr>   <certs> <status>
//				<download>...       --> <downloader>  <certs> <config>
//   <downloader> <certs> <config>  --> <downloader>  <certs> <status>
//				<downloaded>...     --> <downloader>  <certs> <status>
//	 <downloader> <baseos> <status> --> <baseosmgr>   <baseos> <status>

package baseosmgr

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lf-edge/eve/pkg/pillar/agentlog"
	"github.com/lf-edge/eve/pkg/pillar/pidfile"
	"github.com/lf-edge/eve/pkg/pillar/pubsub"
	"github.com/lf-edge/eve/pkg/pillar/types"
	log "github.com/sirupsen/logrus"
)

const (
	agentName      = "baseosmgr"
	partitionCount = 2
	// Time limits for event loop handlers
	errorTime   = 3 * time.Minute
	warningTime = 40 * time.Second
)

// Set from Makefile
var Version = "No version specified"

type baseOsMgrContext struct {
	verifierRestarted        bool // Information from handleVerifierRestarted
	pubBaseOsStatus          *pubsub.Publication
	pubBaseOsDownloadConfig  *pubsub.Publication
	pubBaseOsVerifierConfig  *pubsub.Publication
	pubCertObjStatus         *pubsub.Publication
	pubCertObjDownloadConfig *pubsub.Publication
	pubZbootStatus           *pubsub.Publication

	subGlobalConfig          *pubsub.Subscription
	globalConfig             *types.GlobalConfig
	GCInitialized            bool
	subBaseOsConfig          *pubsub.Subscription
	subZbootConfig           *pubsub.Subscription
	subCertObjConfig         *pubsub.Subscription
	subBaseOsDownloadStatus  *pubsub.Subscription
	subCertObjDownloadStatus *pubsub.Subscription
	subBaseOsVerifierStatus  *pubsub.Subscription
	subNodeAgentStatus       *pubsub.Subscription
	rebootReason             string
	rebootTime               time.Time
}

var debug = false
var debugOverride bool // From command line arg

func Run() {
	versionPtr := flag.Bool("v", false, "Version")
	debugPtr := flag.Bool("d", false, "Debug flag")
	curpartPtr := flag.String("c", "", "Current partition")
	flag.Parse()
	debug = *debugPtr
	debugOverride = debug
	if debugOverride {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	curpart := *curpartPtr
	if *versionPtr {
		fmt.Printf("%s: %s\n", os.Args[0], Version)
		return
	}
	logf, err := agentlog.Init(agentName, curpart)
	if err != nil {
		log.Fatal(err)
	}
	defer logf.Close()
	if err := pidfile.CheckAndCreatePidfile(agentName); err != nil {
		log.Fatal(err)
	}

	log.Infof("Starting %s\n", agentName)

	// Run a periodic timer so we always update StillRunning
	stillRunning := time.NewTicker(25 * time.Second)
	agentlog.StillRunning(agentName, warningTime, errorTime)

	// Context to pass around
	ctx := baseOsMgrContext{
		globalConfig: &types.GlobalConfigDefaults,
	}

	// initialize publishing handles
	initializeSelfPublishHandles(&ctx)

	// initialize module specific subscriber handles
	initializeGlobalConfigHandles(&ctx)
	initializeNodeAgentHandles(&ctx)
	initializeZedagentHandles(&ctx)
	initializeVerifierHandles(&ctx)
	initializeDownloaderHandles(&ctx)

	// publish zboot partition status
	publishZbootPartitionStatusAll(&ctx)

	// report other agents, about, zboot status availability
	ctx.pubZbootStatus.SignalRestarted()

	// Pick up debug aka log level before we start real work
	for !ctx.GCInitialized {
		log.Infof("waiting for GCInitialized")
		select {
		case change := <-ctx.subGlobalConfig.C:
			ctx.subGlobalConfig.ProcessChange(change)
		case <-stillRunning.C:
		}
		agentlog.StillRunning(agentName, warningTime, errorTime)
	}
	log.Infof("processed GlobalConfig")

	// First we process the verifierStatus to avoid downloading
	// an image we already have in place.
	log.Infof("Handling initial verifier Status\n")
	for !ctx.verifierRestarted {
		select {
		case change := <-ctx.subGlobalConfig.C:
			ctx.subGlobalConfig.ProcessChange(change)

		case change := <-ctx.subBaseOsVerifierStatus.C:
			ctx.subBaseOsVerifierStatus.ProcessChange(change)
			if ctx.verifierRestarted {
				log.Infof("Verifier reported restarted\n")
			}

		case change := <-ctx.subNodeAgentStatus.C:
			ctx.subNodeAgentStatus.ProcessChange(change)

		case <-stillRunning.C:
		}
		agentlog.StillRunning(agentName, warningTime, errorTime)
	}

	// start the forever loop for event handling
	for {
		select {
		case change := <-ctx.subGlobalConfig.C:
			ctx.subGlobalConfig.ProcessChange(change)

		case change := <-ctx.subCertObjConfig.C:
			ctx.subCertObjConfig.ProcessChange(change)

		case change := <-ctx.subBaseOsConfig.C:
			ctx.subBaseOsConfig.ProcessChange(change)

		case change := <-ctx.subZbootConfig.C:
			ctx.subZbootConfig.ProcessChange(change)

		case change := <-ctx.subBaseOsDownloadStatus.C:
			ctx.subBaseOsDownloadStatus.ProcessChange(change)

		case change := <-ctx.subBaseOsVerifierStatus.C:
			ctx.subBaseOsVerifierStatus.ProcessChange(change)

		case change := <-ctx.subCertObjDownloadStatus.C:
			ctx.subCertObjDownloadStatus.ProcessChange(change)

		case change := <-ctx.subNodeAgentStatus.C:
			ctx.subNodeAgentStatus.ProcessChange(change)

		case <-stillRunning.C:
		}
		agentlog.StillRunning(agentName, warningTime, errorTime)
	}
}

func handleVerifierRestarted(ctxArg interface{}, done bool) {
	ctx := ctxArg.(*baseOsMgrContext)
	log.Infof("handleVerifierRestarted(%v)\n", done)
	if done {
		ctx.verifierRestarted = true
	}
}

func handleBaseOsConfigDelete(ctxArg interface{}, key string,
	configArg interface{}) {

	log.Infof("handleBaseOsConfigDelete(%s)\n", key)
	ctx := ctxArg.(*baseOsMgrContext)
	status := lookupBaseOsStatus(ctx, key)
	if status == nil {
		log.Infof("handleBaseOsConfigDelete: unknown %s\n", key)
		return
	}
	handleBaseOsDelete(ctx, key, status)
	log.Infof("handleBaseOsConfigDelete(%s) done\n", key)
}

// base os config modify event
func handleBaseOsCreate(ctxArg interface{}, key string, configArg interface{}) {

	log.Infof("handleBaseOsCreate(%s)\n", key)
	ctx := ctxArg.(*baseOsMgrContext)
	config := configArg.(types.BaseOsConfig)
	status := types.BaseOsStatus{
		UUIDandVersion: config.UUIDandVersion,
		BaseOsVersion:  config.BaseOsVersion,
		ConfigSha256:   config.ConfigSha256,
	}

	status.StorageStatusList = make([]types.StorageStatus,
		len(config.StorageConfigList))

	for i, sc := range config.StorageConfigList {
		ss := &status.StorageStatusList[i]
		ss.Name = sc.Name
		ss.ImageSha256 = sc.ImageSha256
		ss.Target = sc.Target
	}
	// Check image count
	err := validateBaseOsConfig(ctx, config)
	if err != nil {
		errStr := fmt.Sprintf("%v", err)
		log.Errorln(errStr)
		status.Error = errStr
		status.ErrorTime = time.Now()
		publishBaseOsStatus(ctx, &status)
		return
	}
	publishBaseOsStatus(ctx, &status)
	baseOsHandleStatusUpdate(ctx, &config, &status)
}

func handleBaseOsModify(ctxArg interface{}, key string, configArg interface{}) {

	log.Infof("handleBaseOsModify(%s)\n", key)
	ctx := ctxArg.(*baseOsMgrContext)
	config := configArg.(types.BaseOsConfig)
	status := lookupBaseOsStatus(ctx, key)
	if status == nil {
		log.Errorf("handleBaseOsModify status not found, ignored %+v\n", key)
		return
	}

	log.Infof("handleBaseOsModify(%s) for %s Activate %v\n",
		config.Key(), config.BaseOsVersion, config.Activate)

	// Check image count
	err := validateBaseOsConfig(ctx, config)
	if err != nil {
		errStr := fmt.Sprintf("%v", err)
		log.Errorln(errStr)
		status.Error = errStr
		status.ErrorTime = time.Now()
		publishBaseOsStatus(ctx, status)
		return
	}

	// update the version field, uuids being the same
	status.UUIDandVersion = config.UUIDandVersion
	publishBaseOsStatus(ctx, status)
	baseOsHandleStatusUpdate(ctx, &config, status)
}

// base os config delete event
func handleBaseOsDelete(ctx *baseOsMgrContext, key string,
	status *types.BaseOsStatus) {

	log.Infof("handleBaseOsDelete for %s\n", status.BaseOsVersion)
	removeBaseOsConfig(ctx, status.Key())
}

func handleCertObjConfigDelete(ctxArg interface{}, key string,
	configArg interface{}) {

	log.Infof("handleCertObjConfigDelete(%s)\n", key)
	ctx := ctxArg.(*baseOsMgrContext)
	status := lookupCertObjStatus(ctx, key)
	if status == nil {
		log.Infof("handleCertObjConfigDelete: unknown %s\n", key)
		return
	}
	handleCertObjDelete(ctx, key, status)
	log.Infof("handleCertObjConfigDelete(%s) done\n", key)
}

// certificate config/status event handlers
// certificate config create event
func handleCertObjCreate(ctxArg interface{}, key string, configArg interface{}) {
	ctx := ctxArg.(*baseOsMgrContext)
	config := configArg.(types.CertObjConfig)
	log.Infof("handleCertObjCreate for %s\n", key)

	status := types.CertObjStatus{
		UUIDandVersion: config.UUIDandVersion,
		ConfigSha256:   config.ConfigSha256,
	}

	status.StorageStatusList = make([]types.StorageStatus,
		len(config.StorageConfigList))

	for i, sc := range config.StorageConfigList {
		ss := &status.StorageStatusList[i]
		ss.Name = sc.Name
		ss.ImageSha256 = sc.ImageSha256
		ss.FinalObjDir = types.CertificateDirname
	}

	publishCertObjStatus(ctx, &status)

	certObjHandleStatusUpdate(ctx, &config, &status)
}

// certificate config modify event
func handleCertObjModify(ctxArg interface{}, key string, configArg interface{}) {
	ctx := ctxArg.(*baseOsMgrContext)
	config := configArg.(types.CertObjConfig)
	status := lookupCertObjStatus(ctx, key)
	uuidStr := config.Key()
	log.Infof("handleCertObjModify for %s\n", uuidStr)

	if config.UUIDandVersion.Version != status.UUIDandVersion.Version {
		log.Infof("handleCertObjModify(%s), New config version %v\n", key,
			config.UUIDandVersion.Version)
		status.UUIDandVersion = config.UUIDandVersion
		publishCertObjStatus(ctx, status)

	}

	// on storage config change, purge and recreate
	if certObjCheckConfigModify(ctx, key, &config, status) {
		removeCertObjConfig(ctx, key)
		handleCertObjCreate(ctx, key, config)
	}
}

// certificate config delete event
func handleCertObjDelete(ctx *baseOsMgrContext, key string,
	status *types.CertObjStatus) {

	uuidStr := status.Key()
	log.Infof("handleCertObjDelete for %s\n", uuidStr)
	removeCertObjConfig(ctx, uuidStr)
}

// base os/certs download status modify event
// Handles both create and modify events
func handleDownloadStatusModify(ctxArg interface{}, key string,
	statusArg interface{}) {

	status := statusArg.(types.DownloaderStatus)
	ctx := ctxArg.(*baseOsMgrContext)
	log.Infof("handleDownloadStatusModify for %s\n",
		status.Safename)
	updateDownloaderStatus(ctx, &status)
}

// base os/certs download status delete event
func handleDownloadStatusDelete(ctxArg interface{}, key string,
	statusArg interface{}) {

	status := statusArg.(types.DownloaderStatus)
	log.Infof("handleDownloadStatusDelete RefCount %d Expired %v for %s\n",
		status.RefCount, status.Expired, key)
	// Nothing to do
}

// base os verifier status modify event
// Handles both create and modify events
func handleVerifierStatusModify(ctxArg interface{}, key string,
	statusArg interface{}) {

	status := statusArg.(types.VerifyImageStatus)
	ctx := ctxArg.(*baseOsMgrContext)
	log.Infof("handleVerifierStatusModify for %s\n", status.Safename)
	updateVerifierStatus(ctx, &status)
}

// base os verifier status delete event
func handleVerifierStatusDelete(ctxArg interface{}, key string,
	statusArg interface{}) {

	status := statusArg.(types.VerifyImageStatus)
	log.Infof("handleVeriferStatusDelete RefCount %d Expired %v for %s\n",
		status.RefCount, status.Expired, key)
	// Nothing to do
}

func appendError(allErrors string, prefix string, lasterr string) string {
	return fmt.Sprintf("%s%s: %s\n\n", allErrors, prefix, lasterr)
}

// This handles both the create and modify events
func handleGlobalConfigModify(ctxArg interface{}, key string,
	statusArg interface{}) {

	ctx := ctxArg.(*baseOsMgrContext)
	if key != "global" {
		log.Infof("handleGlobalConfigModify: ignoring %s\n", key)
		return
	}
	var gcp *types.GlobalConfig
	debug, gcp = agentlog.HandleGlobalConfig(ctx.subGlobalConfig, agentName,
		debugOverride)
	if gcp != nil {
		ctx.globalConfig = gcp
		ctx.GCInitialized = true
	}
	log.Infof("handleGlobalConfigModify done for %s\n", key)
}

func handleGlobalConfigDelete(ctxArg interface{}, key string,
	statusArg interface{}) {

	ctx := ctxArg.(*baseOsMgrContext)
	if key != "global" {
		log.Infof("handleGlobalConfigDelete: ignoring %s\n", key)
		return
	}
	log.Infof("handleGlobalConfigDelete for %s\n", key)
	debug, _ = agentlog.HandleGlobalConfig(ctx.subGlobalConfig, agentName,
		debugOverride)
	*ctx.globalConfig = types.GlobalConfigDefaults
	log.Infof("handleGlobalConfigDelete done for %s\n", key)
}

func initializeSelfPublishHandles(ctx *baseOsMgrContext) {
	pubBaseOsStatus, err := pubsub.Publish(agentName,
		types.BaseOsStatus{})
	if err != nil {
		log.Fatal(err)
	}
	pubBaseOsStatus.ClearRestarted()
	ctx.pubBaseOsStatus = pubBaseOsStatus

	pubBaseOsDownloadConfig, err := pubsub.PublishScope(agentName,
		types.BaseOsObj, types.DownloaderConfig{})
	if err != nil {
		log.Fatal(err)
	}
	pubBaseOsDownloadConfig.ClearRestarted()
	ctx.pubBaseOsDownloadConfig = pubBaseOsDownloadConfig

	pubBaseOsVerifierConfig, err := pubsub.PublishScope(agentName,
		types.BaseOsObj, types.VerifyImageConfig{})
	if err != nil {
		log.Fatal(err)
	}
	pubBaseOsVerifierConfig.ClearRestarted()
	ctx.pubBaseOsVerifierConfig = pubBaseOsVerifierConfig

	pubCertObjStatus, err := pubsub.Publish(agentName,
		types.CertObjStatus{})
	if err != nil {
		log.Fatal(err)
	}
	pubCertObjStatus.ClearRestarted()
	ctx.pubCertObjStatus = pubCertObjStatus

	pubCertObjDownloadConfig, err := pubsub.PublishScope(agentName,
		types.CertObj, types.DownloaderConfig{})
	if err != nil {
		log.Fatal(err)
	}
	pubCertObjDownloadConfig.ClearRestarted()
	ctx.pubCertObjDownloadConfig = pubCertObjDownloadConfig

	pubZbootStatus, err := pubsub.Publish(agentName, types.ZbootStatus{})
	if err != nil {
		log.Fatal(err)
	}
	pubZbootStatus.ClearRestarted()
	ctx.pubZbootStatus = pubZbootStatus
}

func initializeGlobalConfigHandles(ctx *baseOsMgrContext) {

	// Look for global config such as log levels
	subGlobalConfig, err := pubsub.Subscribe("", types.GlobalConfig{},
		false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subGlobalConfig.MaxProcessTimeWarn = warningTime
	subGlobalConfig.MaxProcessTimeError = errorTime
	subGlobalConfig.ModifyHandler = handleGlobalConfigModify
	subGlobalConfig.CreateHandler = handleGlobalConfigModify
	subGlobalConfig.DeleteHandler = handleGlobalConfigDelete
	ctx.subGlobalConfig = subGlobalConfig
	subGlobalConfig.Activate()
}

func initializeNodeAgentHandles(ctx *baseOsMgrContext) {
	// Look for NodeAgentStatus, from zedagent
	subNodeAgentStatus, err := pubsub.Subscribe("nodeagent",
		types.NodeAgentStatus{}, false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subNodeAgentStatus.MaxProcessTimeWarn = warningTime
	subNodeAgentStatus.MaxProcessTimeError = errorTime
	subNodeAgentStatus.ModifyHandler = handleNodeAgentStatusModify
	subNodeAgentStatus.DeleteHandler = handleNodeAgentStatusDelete
	ctx.subNodeAgentStatus = subNodeAgentStatus
	subNodeAgentStatus.Activate()

	// Look for ZbootConfig, from nodeagent
	subZbootConfig, err := pubsub.Subscribe("nodeagent",
		types.ZbootConfig{}, false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subZbootConfig.MaxProcessTimeWarn = warningTime
	subZbootConfig.MaxProcessTimeError = errorTime
	subZbootConfig.ModifyHandler = handleZbootConfigModify
	subZbootConfig.CreateHandler = handleZbootConfigModify
	subZbootConfig.DeleteHandler = handleZbootConfigDelete
	ctx.subZbootConfig = subZbootConfig
	subZbootConfig.Activate()
}

func initializeZedagentHandles(ctx *baseOsMgrContext) {
	// Look for BaseOsConfig , from zedagent
	subBaseOsConfig, err := pubsub.Subscribe("zedagent",
		types.BaseOsConfig{}, false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subBaseOsConfig.MaxProcessTimeWarn = warningTime
	subBaseOsConfig.MaxProcessTimeError = errorTime
	subBaseOsConfig.ModifyHandler = handleBaseOsModify
	subBaseOsConfig.CreateHandler = handleBaseOsCreate
	subBaseOsConfig.DeleteHandler = handleBaseOsConfigDelete
	ctx.subBaseOsConfig = subBaseOsConfig
	subBaseOsConfig.Activate()

	// Look for CertObjConfig, from zedagent
	subCertObjConfig, err := pubsub.Subscribe("zedagent",
		types.CertObjConfig{}, false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subCertObjConfig.MaxProcessTimeWarn = warningTime
	subCertObjConfig.MaxProcessTimeError = errorTime
	subCertObjConfig.ModifyHandler = handleCertObjModify
	subCertObjConfig.CreateHandler = handleCertObjCreate
	subCertObjConfig.DeleteHandler = handleCertObjConfigDelete
	ctx.subCertObjConfig = subCertObjConfig
	subCertObjConfig.Activate()
}

func initializeDownloaderHandles(ctx *baseOsMgrContext) {
	// Look for BaseOs DownloaderStatus from downloader
	subBaseOsDownloadStatus, err := pubsub.SubscribeScope("downloader",
		types.BaseOsObj, types.DownloaderStatus{}, false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subBaseOsDownloadStatus.MaxProcessTimeWarn = warningTime
	subBaseOsDownloadStatus.MaxProcessTimeError = errorTime
	subBaseOsDownloadStatus.ModifyHandler = handleDownloadStatusModify
	subBaseOsDownloadStatus.CreateHandler = handleDownloadStatusModify
	subBaseOsDownloadStatus.DeleteHandler = handleDownloadStatusDelete
	ctx.subBaseOsDownloadStatus = subBaseOsDownloadStatus
	subBaseOsDownloadStatus.Activate()

	// Look for Certs DownloaderStatus from downloader
	subCertObjDownloadStatus, err := pubsub.SubscribeScope("downloader",
		types.CertObj, types.DownloaderStatus{}, false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subCertObjDownloadStatus.MaxProcessTimeWarn = warningTime
	subCertObjDownloadStatus.MaxProcessTimeError = errorTime
	subCertObjDownloadStatus.ModifyHandler = handleDownloadStatusModify
	subCertObjDownloadStatus.CreateHandler = handleDownloadStatusModify
	subCertObjDownloadStatus.DeleteHandler = handleDownloadStatusDelete
	ctx.subCertObjDownloadStatus = subCertObjDownloadStatus
	subCertObjDownloadStatus.Activate()

}

func initializeVerifierHandles(ctx *baseOsMgrContext) {
	// Look for VerifyImageStatus from verifier
	subBaseOsVerifierStatus, err := pubsub.SubscribeScope("verifier",
		types.BaseOsObj, types.VerifyImageStatus{}, false, ctx)
	if err != nil {
		log.Fatal(err)
	}
	subBaseOsVerifierStatus.MaxProcessTimeWarn = warningTime
	subBaseOsVerifierStatus.MaxProcessTimeError = errorTime
	subBaseOsVerifierStatus.ModifyHandler = handleVerifierStatusModify
	subBaseOsVerifierStatus.CreateHandler = handleVerifierStatusModify
	subBaseOsVerifierStatus.DeleteHandler = handleVerifierStatusDelete
	subBaseOsVerifierStatus.RestartHandler = handleVerifierRestarted
	ctx.subBaseOsVerifierStatus = subBaseOsVerifierStatus
	subBaseOsVerifierStatus.Activate()
}

// This handles both the create and modify events
func handleNodeAgentStatusModify(ctxArg interface{}, key string,
	statusArg interface{}) {
	ctx := ctxArg.(*baseOsMgrContext)
	status := statusArg.(types.NodeAgentStatus)
	ctx.rebootTime = status.RebootTime
	ctx.rebootReason = status.RebootReason
	updateBaseOsStatusOnReboot(ctx)
	log.Infof("handleNodeAgentStatusModify(%s) done\n", key)
}

func handleNodeAgentStatusDelete(ctxArg interface{}, key string,
	statusArg interface{}) {
	// do nothing
	log.Infof("handleNodeAgentStatusDelete(%s) done\n", key)
}

// This handles both the create and modify events
func handleZbootConfigModify(ctxArg interface{}, key string, configArg interface{}) {
	ctx := ctxArg.(*baseOsMgrContext)
	config := configArg.(types.ZbootConfig)
	status := getZbootStatus(ctx, key)
	if status == nil {
		log.Infof("handleZbootConfigModify: unknown %s\n", key)
		return
	}
	log.Infof("handleZbootModify for %s TestComplete %v\n",
		config.Key(), config.TestComplete)

	if config.TestComplete != status.TestComplete {
		handleZbootTestComplete(ctx, config, *status)
	}

	log.Infof("handleZbootConfigModify(%s) done\n", key)
}

func handleZbootConfigDelete(ctxArg interface{}, key string,
	configArg interface{}) {

	log.Infof("handleZbootConfigDelete(%s)\n", key)
	ctx := ctxArg.(*baseOsMgrContext)
	status := getZbootStatus(ctx, key)
	if status == nil {
		log.Infof("handleZbootConfigDelete: unknown %s\n", key)
		return
	}
	// Nothing to do. We report ZbootStatus for the IMG* partitions
	// in any case
	log.Infof("handleZbootConfigDelete(%s) done\n", key)
}
