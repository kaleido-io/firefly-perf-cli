// Copyright © 2022 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package perf

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/go-resty/resty/v2"
	"github.com/hyperledger/firefly-common/pkg/fftypes"
	"github.com/hyperledger/firefly-perf-cli/internal/conf"
	"github.com/hyperledger/firefly-perf-cli/internal/util"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

const workerPrefix = "worker-"

var mutex = &sync.Mutex{}
var TRANSPORT_TYPE = "websockets"

var METRICS_NAMESPACE = "ffperf"
var METRICS_SUBSYSTEM = "runner"

var totalActionsCounter = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: METRICS_NAMESPACE,
	Name:      "actions_submitted_total",
	Subsystem: METRICS_SUBSYSTEM,
})

var failedTransactionsCounter = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: METRICS_NAMESPACE,
	Name:      "transactions_failed_total",
	Subsystem: METRICS_SUBSYSTEM,
})

var receivedEventsCounter = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: METRICS_NAMESPACE,
	Name:      "received_events_total",
	Subsystem: METRICS_SUBSYSTEM,
})

var perfTestDurationHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: METRICS_NAMESPACE,
	Subsystem: METRICS_SUBSYSTEM,
	Name:      "perf_test_duration_seconds",
	Buckets:   []float64{1.0, 2.0, 5.0, 10.0, 30.0},
}, []string{"test"})

func Init() {
	prometheus.Register(receivedEventsCounter)
	prometheus.Register(totalActionsCounter)
	prometheus.Register(perfTestDurationHistogram)
	prometheus.Register(failedTransactionsCounter)
}

func getMetricVal(collector prometheus.Collector) float64 {
	collectorChannel := make(chan prometheus.Metric, 1)
	collector.Collect(collectorChannel)
	metric := dto.Metric{}
	err := (<-collectorChannel).Write(&metric)
	if err != nil {
		log.Errorf("error writing metric: %s", err)
	}
	if metric.Counter != nil {
		return *metric.Counter.Value
	} else if metric.Gauge != nil {
		return *metric.Gauge.Value
	}
	return 0
}

type PerfRunner interface {
	Init() error
	Start() error
}

type TrackingIDType string

const (
	TrackingIDTypeMessageID    TrackingIDType = "Message ID"
	TrackingIDTypeTransferID   TrackingIDType = "Token Transfer ID"
	TrackingIDTypeWorkerNumber                = "Worker ID"
)

type TestCase interface {
	WorkerID() int
	RunOnce(iterationCount int) (trackingID string, err error)
	IDType() TrackingIDType
	Name() string
	ActionsPerLoop() int
}

type inflightTest struct {
	time     time.Time
	testCase TestCase
}

type summary struct {
	mutex        *sync.Mutex
	rampSummary  int64
	totalSummary int64
}
type perfRunner struct {
	bfr      chan int
	cfg      *conf.RunnerConfig
	client   *resty.Client
	ctx      context.Context
	shutdown context.CancelFunc
	stopping bool

	startTime     int64
	endSendTime   int64
	endTime       int64
	startRampTime int64
	endRampTime   int64

	reportBuilder *util.Report
	sendTime      *util.Latency
	receiveTime   *util.Latency
	totalTime     *util.Latency
	summary       summary
	msgTimeMap    sync.Map

	pollingTickers             map[string]*time.Ticker
	completedTransactions      map[string]chan string
	eventPrefixForCurrentStage string
	nodeURLs                   []string
	daemon                     bool
	sender                     string
	totalWorkers               int
	txIDMap                    sync.Map
}

func New(config *conf.RunnerConfig, reportBuilder *util.Report) PerfRunner {
	if config.LogLevel != "" {
		if level, err := log.ParseLevel(config.LogLevel); err == nil {
			log.SetLevel(level)
		}
	}

	totalWorkers := 0
	for _, test := range config.Tests {
		totalWorkers += test.Workers
	}

	// Create channel based dispatch for workers
	completedTransactions := make(map[string]chan string)
	for i := 0; i < totalWorkers; i++ {
		preFixedWorkerID := fmt.Sprintf("%s%d", workerPrefix, i)
		completedTransactions[preFixedWorkerID] = make(chan string)
	}

	ctx, cancel := context.WithCancel(context.Background())

	startRampTime := time.Now().Unix()
	endRampTime := time.Now().Unix() + int64(config.RampLength.Seconds())
	startTime := endRampTime
	endTime := startTime + int64(config.Length.Seconds())

	pr := &perfRunner{
		bfr:           make(chan int, totalWorkers),
		cfg:           config,
		ctx:           ctx,
		shutdown:      cancel,
		startRampTime: startRampTime,
		endRampTime:   endRampTime,
		startTime:     startTime,
		endTime:       endTime,
		reportBuilder: reportBuilder,
		sendTime:      &util.Latency{},
		receiveTime:   &util.Latency{},
		totalTime:     &util.Latency{},
		msgTimeMap:    sync.Map{},
		summary: summary{
			totalSummary: 0,
			mutex:        &sync.Mutex{},
		},
		completedTransactions:      completedTransactions,
		eventPrefixForCurrentStage: workerPrefix, // most test case currently doesn't have a prep stage, so default to test running stage prefix
		nodeURLs:                   config.NodeURLs,
		daemon:                     config.Daemon,
		sender:                     config.SenderURL,
		totalWorkers:               totalWorkers,
		txIDMap:                    sync.Map{},
	}
	return pr
}

func (pr *perfRunner) Init() (err error) {
	pr.client = getPaladinClient(pr.sender)
	pr.client.
		SetRetryCount(10).
		// You can override initial retry wait time.
		// Default is 100 milliseconds.
		SetRetryWaitTime(1 * time.Second).
		// MaxWaitTime can be overridden as well.
		// Default is 2 seconds.
		SetRetryMaxWaitTime(30 * time.Second).
		AddRetryCondition(
			// RetryConditionFunc type is for retry condition function
			// input: non-nil Response OR request execution error
			func(r *resty.Response, err error) bool {
				if r.IsError() || err != nil {
					if r.StatusCode() == 409 {
						// Do not retry on duplicates, because FireFly should already be processing the transaction
						return false
					}
					// Retry for all other errors
					log.Warnf("Retrying HTTP request. Status: '%v' Request: '%s' '%s' Response: '%s' Error: '%v'", r.Status(), r.Request.Method, r.Request.URL, r.Body(), r.Error())
					return true
				}
				return false
			},
		)
	return nil
}

func (pr *perfRunner) Start() (err error) {
	log.Infof("Running test:\n%+v", pr.cfg)

	pr.pollingTickers = make(map[string]*time.Ticker)

	for _, nodeURL := range pr.nodeURLs {
		go func(nodeURL string) {
			pr.pollingLoop(nodeURL)
		}(nodeURL)
		// TODO AM: this wouldn't actually work properly if there was more than one node
		// but I don't think we really need to support multiple nodes for this test
		go func(nodeURL string) {
			pr.findLostTransactions(nodeURL)
		}(nodeURL)
	}

	id := 0
	for _, test := range pr.cfg.Tests {
		log.Infof("Starting %d workers for case \"%s\"", test.Workers, test.Name)
		for iWorker := 0; iWorker < test.Workers; iWorker++ {
			var tc TestCase = newCustomEthereumTestWorker(pr, id, test.ActionsPerLoop)
			delayPerWorker := pr.cfg.RampLength / time.Duration(test.Workers)

			go func(i int) {
				// Delay the start of the next worker by (ramp time) / (number of workers)
				if delayPerWorker > 0 {
					time.Sleep(delayPerWorker * time.Duration(i))
					log.Infof("Ramping up. Starting next worker after waiting %v", delayPerWorker)
				}
				err := pr.runLoop(tc)
				if err != nil {
					log.Errorf("Worker %d failed: %s", tc.WorkerID(), err)
				}
			}(iWorker)
			id++
		}
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt)
	// signal.Notify(signalCh, os.Kill)
	signal.Notify(signalCh, syscall.SIGTERM)
	signal.Notify(signalCh, syscall.SIGQUIT)
	// signal.Notify(signalCh, syscall.SIGKILL)

	i := 0
	lastCheckedTime := time.Now()

	rateLimiter := rate.NewLimiter(rate.Limit(math.MaxFloat64), math.MaxInt)

	if pr.cfg.MaxSubmissionsPerSecond > 0 {
		rateLimiter = rate.NewLimiter(rate.Limit(pr.cfg.MaxSubmissionsPerSecond), pr.cfg.MaxSubmissionsPerSecond)
	}
	log.Infof("Sending rate: %f per second with %d burst", rateLimiter.Limit(), rateLimiter.Burst())
perfLoop:
	for pr.daemon || time.Now().Unix() < pr.endTime {
		timeout := time.After(60 * time.Second)
		// If we've been given a maximum number of actions to perform, check if we're done
		if pr.cfg.MaxActions > 0 && int64(getMetricVal(totalActionsCounter)) >= pr.cfg.MaxActions {
			break perfLoop
		}

		select {
		case <-signalCh:
			break perfLoop
		case pr.bfr <- i:
			err = rateLimiter.Wait(pr.ctx)
			if err != nil {
				log.Panic(fmt.Errorf("rate limiter failed"))
				break perfLoop
			}
			i++
			if time.Since(lastCheckedTime).Seconds() > pr.cfg.MaxTimePerAction.Seconds() {
				if pr.detectDelinquentMsgs() && pr.cfg.DelinquentAction == conf.DelinquentActionExit.String() {
					break perfLoop
				}
				lastCheckedTime = time.Now()
			}
		case <-timeout:
			if pr.detectDelinquentMsgs() && pr.cfg.DelinquentAction == conf.DelinquentActionExit.String() {
				break perfLoop
			}
			lastCheckedTime = time.Now()
		}

	}

	pr.stopping = true

	idleStart := time.Now()

	if pr.cfg.NoWaitSubmission {
		eventsCount := getMetricVal(receivedEventsCounter)
		submissionCount := getMetricVal(totalActionsCounter)
		previousEventCount := eventsCount
		log.Infof("<No wait submission mode> Wait for the event count %f to reach request sent count %f", eventsCount, submissionCount)
		for {
			previousEventCount = eventsCount
			eventsCount = getMetricVal(receivedEventsCounter)
			if previousEventCount < eventsCount {
				// reset idle start time if there are new events
				idleStart = time.Now()
			}
			if eventsCount == submissionCount {
				break
			} else if eventsCount > submissionCount {
				log.Warnf("The number of events received %f is greater than the number of requests sent %f.", eventsCount, submissionCount)
				break
			}

			// Check if more than 30 seconds has passed
			if time.Since(idleStart) > 30*time.Second {
				log.Errorf("The number of events received %f doesn't tally up to the number of requests sent %f after 30s idle time, total tally time: %s.", eventsCount, submissionCount, time.Since(time.Unix(pr.startTime, 0)))
				break
			}

			time.Sleep(time.Second * 1)
			log.Infof("<No wait submission mode> Wait for the event count %f to reach request sent count %f", eventsCount, submissionCount)
		}
	}

	measuredActions := pr.summary.totalSummary
	measuredTime := time.Since(time.Unix(pr.startTime, 0))

	testNames := make([]string, len(pr.cfg.Tests))
	for i, t := range pr.cfg.Tests {
		testNames[i] = t.Name.String()
	}
	testNameString := testNames[0]
	if len(testNames) > 1 {
		testNameString = strings.Join(testNames[:], ",")
	}
	tps := util.GenerateTPS(measuredActions, pr.startTime, pr.endSendTime)
	pr.reportBuilder.AddTestRunMetrics(testNameString, measuredActions, measuredTime, tps, pr.totalTime)
	err = pr.reportBuilder.GenerateHTML()

	if err != nil {
		log.Errorf("failed to generate performance report: %+v", err)
	}

	// we sleep on shutdown / completion to allow for Prometheus metrics to be scraped one final time
	// After 30 seconds workers should be completed, so we check for delinquent messages
	// one last time so metrics are up-to-date
	log.Warn("Runner stopping in 5s")
	time.Sleep(5 * time.Second)
	pr.detectDelinquentMsgs()

	log.Info("Cleaning up")

	pr.cleanup()

	log.Info("Shutdown summary:")
	log.Infof(" - Prometheus metric received_events_total   = %f\n", getMetricVal(receivedEventsCounter))
	log.Infof(" - Prometheus metric failed_transactions_total = %f\n", getMetricVal(failedTransactionsCounter))
	log.Infof(" - Prometheus metric actions_submitted_total = %f\n", getMetricVal(totalActionsCounter))
	log.Infof(" - Test duration: %s", measuredTime)
	log.Infof(" - Measured actions: %d", measuredActions)
	log.Infof(" - Measured send TPS: %2f", tps.SendRate)
	log.Infof(" - Measured throughput: %2f", tps.Throughput)
	log.Infof(" - Measured send duration: %s", pr.sendTime)
	if !pr.cfg.NoWaitSubmission {
		log.Infof(" - Measured event receiving duration: %s", pr.receiveTime)
	}
	log.Infof(" - Measured total duration: %s", pr.totalTime)

	return nil
}

func (pr *perfRunner) cleanup() {
	for _, nodeURL := range pr.nodeURLs {
		pr.pollingTickers[nodeURL].Stop()
	}
}

// TODO AM: do we actually need the tags?
type transaction struct {
	ID             string `json:"id"`
	IdempotencyKey string `json:"idempotencyKey"`
	Public         []struct {
		Success     *bool  `json:"success,omitempty"`
		CompletedAt string `json:"completedAt"`
	} `json:"public,omitempty"`
}

type queryTransactionsResponse struct {
	Result []transaction
}

type getTransactionResponse struct {
	Result transaction
}

func (pr *perfRunner) pollingLoop(nodeURL string) {
	log.Infof("Polling loop started for %s...", nodeURL)
	// last completed time - needs to be a string - set to the current time - as long as this is before submission starts
	// the milliseconds all end up as zeros but this is accurate enough to avoid picking up transactions from previous test runs
	lastCompletedTime := time.Now().Format("2006-1-2T15:04:05.000000000Z")
	client := getPaladinClient(nodeURL)
	ticker := time.NewTicker(3 * time.Second) // TODO AM: make this configurable
	pr.pollingTickers[nodeURL] = ticker
	for {
		select {
		case <-ticker.C:
			// query transactions since the last completed time - sorted by created time
			// if completed
			// get the worker id from the idempotency key
			// put a message on the correct worker channel
			payload := fmt.Sprintf(`{
				"jsonrpc": "2.0",
				"id": "1",
				"method": "ptx_queryTransactionsFull",
				"params": [
					{
						"limit": 1000,
						"gt": [
							{
								"field": "created",
								"value": "%s"
							}
						],
						"sort": [
							"created ASC"
						]
					}
				]
			}`, lastCompletedTime)
			var response queryTransactionsResponse
			var resError string
			res, err := client.R().
				SetHeaders(map[string]string{
					"Accept":       "application/json",
					"Content-Type": "application/json",
				}).
				SetBody([]byte(payload)).
				SetResult(&response).
				SetError(resError).
				Post("/")
			if err != nil || res.IsError() {
				log.Errorf("Error querying transactions")
				break // don't exit- let it try again on the next turn
			}
			// assume transactions were all submitted in their created order so if we find one that hasn't errored/completed
			// there's no point in looking at the next ones
			log.Debugf("New transactions %d", len(response.Result))
			for _, transaction := range response.Result {
				if len(transaction.Public) == 0 || transaction.Public[0].Success == nil {
					break
				}

				if _, ok := pr.txIDMap.LoadAndDelete(transaction.ID); ok {
					// currently ignoring whether the transaction has succeeded or not
					// TODO AM: set up counter for failed submissions as reverted without error seems to be the problem
					workerID, _ := strconv.Atoi(transaction.IdempotencyKey[11:16])
					receivedEventsCounter.Inc()
					if !*transaction.Public[0].Success {
						failedTransactionsCounter.Inc()
					}
					lastCompletedTime = transaction.Public[0].CompletedAt
					if !pr.stopping && workerID >= 0 {
						preFixedWorkerID := fmt.Sprintf("%s%d", pr.eventPrefixForCurrentStage, workerID)
						pr.completedTransactions[preFixedWorkerID] <- nodeURL
					}
					pr.recordCompletedAction()
					pr.summary.mutex.Lock()
					pr.calculateCurrentTps(true)
					pr.summary.mutex.Unlock()
				}
			}
		case <-pr.ctx.Done():
			log.Warnf("Run loop exiting (context cancelled)")
			ticker.Stop()
			return
		}
	}
}

func (pr *perfRunner) allActionsComplete() bool {
	if pr.cfg.MaxActions > 0 && int64(getMetricVal(totalActionsCounter)) >= pr.cfg.MaxActions {
		return true
	}
	return false
}

func (pr *perfRunner) runLoop(tc TestCase) error {
	idType := tc.IDType()
	testName := tc.Name()
	workerID := tc.WorkerID()
	preFixedWorkerID := fmt.Sprintf("%s%d", workerPrefix, workerID)

	loop := 0
	for {
		select {
		case <-pr.bfr:
			var confirmationsPerAction int
			var actionsCompleted int

			// Worker sends its task
			hist, histErr := perfTestDurationHistogram.GetMetricWith(prometheus.Labels{
				"test": testName,
			})

			if histErr != nil {
				log.Errorf("Error retrieving histogram: %s", histErr)
			}

			startTime := time.Now()

			type ActionResponse struct {
				trackingID string
				err        error
			}

			actionResponses := make(chan *ActionResponse, tc.ActionsPerLoop())

			var sentTime time.Time
			var submissionSecondsPerLoop float64
			var eventReceivingSecondsPerLoop float64
			trackingIDs := make([]string, 0)

			for actionsCompleted = 0; actionsCompleted < tc.ActionsPerLoop(); actionsCompleted++ {
				if pr.allActionsComplete() {
					break
				}
				actionCount := actionsCompleted
				go func() {
					trackingID, err := tc.RunOnce(actionCount)
					log.Debugf("%d --> %s action %d sent after %f seconds", workerID, testName, actionCount, time.Since(startTime).Seconds())
					actionResponses <- &ActionResponse{
						trackingID: trackingID,
						err:        err,
					}
				}()
			}
			resultCount := 0
			for {
				aResponse := <-actionResponses
				resultCount++
				if aResponse.err != nil {
					if pr.cfg.DelinquentAction == conf.DelinquentActionExit.String() {
						return aResponse.err
					} else {
						log.Errorf("Worker %d error running job (logging but continuing): %s", workerID, aResponse.err)
					}
				} else {
					trackingIDs = append(trackingIDs, aResponse.trackingID)
					pr.markTestInFlight(tc, aResponse.trackingID)
					log.Debugf("%d --> %s Sent %s: %s", workerID, testName, idType, aResponse.trackingID)
					totalActionsCounter.Inc()
				}
				// if we've reached the expected amount of metadata calls then stop
				if resultCount == tc.ActionsPerLoop() {
					submissionDurationPerLoop := time.Since(startTime)
					pr.sendTime.Record(submissionDurationPerLoop)
					submissionSecondsPerLoop = submissionDurationPerLoop.Seconds()
					sentTime = time.Now()
					log.Debugf("%d --> %s All actions sent %d after %f seconds", workerID, testName, resultCount, submissionSecondsPerLoop)

					pr.endSendTime = time.Now().Unix()
					break
				}
			}
			if pr.cfg.NoWaitSubmission || (testName == conf.PerfTestTokenMint.String() && pr.cfg.SkipMintConfirmations) {
				// For minting tests a worker can (if configured) skip waiting for a matching response event
				// before making itself available for the next job
				confirmationsPerAction = 0
			} else {
				confirmationsPerAction = len(pr.nodeURLs) // TODO AM: is this the right number when polling?
				if testName == conf.PerfTestBlobPrivateMsg.String() || testName == conf.PerfTestPrivateMsg.String() {
					confirmationsPerAction = 2
				}
			}

			// Wait for worker to confirm the message before proceeding to next task

			for j := 0; j < actionsCompleted; j++ {
				var nextTrackingID string
				for i := 0; i < confirmationsPerAction; i++ {
					select {
					case <-pr.ctx.Done():
						return nil
					case <-pr.completedTransactions[preFixedWorkerID]:
						continue
					}
				}
				if len(trackingIDs) > 0 {
					nextTrackingID = trackingIDs[0]
					trackingIDs = trackingIDs[1:]
					pr.stopTrackingRequest(nextTrackingID)
				}
			}
			totalDurationPerLoop := time.Since(startTime)
			pr.totalTime.Record(totalDurationPerLoop)
			secondsPerLoop := totalDurationPerLoop.Seconds()

			if pr.cfg.NoWaitSubmission {
				log.Infof("%d <-- %s Finished (loop=%d) after %f seconds", workerID, testName, loop, secondsPerLoop)
			} else {
				eventReceivingDurationPerLoop := time.Since(sentTime)
				eventReceivingSecondsPerLoop = eventReceivingDurationPerLoop.Seconds()
				pr.receiveTime.Record(totalDurationPerLoop)

				total := submissionSecondsPerLoop + eventReceivingSecondsPerLoop
				subPortion := int((submissionSecondsPerLoop / total) * 100)
				envPortion := int((eventReceivingSecondsPerLoop / total) * 100)
				log.Infof("%d <-- %s Finished (loop=%d), submission time: %f s, event receive time: %f s. Ratio (%d/%d) after %f seconds", workerID, testName, loop, submissionSecondsPerLoop, eventReceivingSecondsPerLoop, subPortion, envPortion, secondsPerLoop)
			}

			if histErr == nil {
				log.Debugf("%d <-- %s Emmiting (loop=%d) after %f seconds", workerID, testName, loop, secondsPerLoop)

				hist.Observe(secondsPerLoop)
			}
			loop++

		case <-pr.ctx.Done():
			return nil
		}
	}
}

func getPaladinClient(node string) *resty.Client {
	client := resty.New()
	client.SetBaseURL(node)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	return client
}

func (pr *perfRunner) detectDelinquentMsgs() bool {
	delinquentMsgs := make(map[string]time.Time)
	pr.msgTimeMap.Range(func(k, v interface{}) bool {
		trackingID := k.(string)
		inflight := v.(*inflightTest)
		if time.Since(inflight.time).Seconds() > pr.cfg.MaxTimePerAction.Seconds() {
			delinquentMsgs[trackingID] = inflight.time
		}
		return true
	})
	incompleteTransactions := 0
	pr.txIDMap.Range(func(k, v interface{}) bool {
		incompleteTransactions++
		return true
	})

	dw, err := json.MarshalIndent(delinquentMsgs, "", "  ")
	if err != nil {
		log.Errorf("Error printing delinquent messages: %s", err)
		return len(delinquentMsgs) > 0
	}

	if len(delinquentMsgs) > 0 {
		log.Warnf("Delinquent Messages:\n%s", string(dw))
	}

	if incompleteTransactions > 0 {
		log.Warnf("Incomplete transactions:\n%d", incompleteTransactions)
	}

	return len(delinquentMsgs) > 0
}

func (pr *perfRunner) findLostTransactions(nodeURL string) {
	ticker := time.NewTicker(5 * time.Second)
	client := getPaladinClient(nodeURL)
	for {
		select {
		case <-ticker.C:
			pr.txIDMap.Range(func(k, v interface{}) bool {
				transactionID := k.(string)
				submittedTime := v.(time.Time)
				if time.Now().After(submittedTime.Add(5 * time.Second)) {
					payload := fmt.Sprintf(`{
						"jsonrpc": "2.0",
						"id": "1",
						"method": "ptx_getTransactionFull",
						"params": ["%s"]
					}`, transactionID)
					var response getTransactionResponse
					var resError string
					res, err := client.R().
						SetHeaders(map[string]string{
							"Accept":       "application/json",
							"Content-Type": "application/json",
						}).
						SetBody([]byte(payload)).
						SetResult(&response).
						SetError(resError).
						Post("/")
					if err != nil || res.IsError() {
						log.Errorf("Error getting transaction")
					}
					transaction := response.Result
					if len(transaction.Public) > 0 && transaction.Public[0].Success != nil {
						log.Debugf("Found lost transaction id %s", transactionID)
						workerID, _ := strconv.Atoi(transaction.IdempotencyKey[11:16])
						receivedEventsCounter.Inc()
						if !*transaction.Public[0].Success {
							failedTransactionsCounter.Inc()
						}
						if !pr.stopping && workerID >= 0 {
							preFixedWorkerID := fmt.Sprintf("%s%d", pr.eventPrefixForCurrentStage, workerID)
							pr.completedTransactions[preFixedWorkerID] <- nodeURL
						}
						pr.txIDMap.Delete(transactionID)
						pr.recordCompletedAction()
						pr.summary.mutex.Lock()
						pr.calculateCurrentTps(true)
						pr.summary.mutex.Unlock()
					}
				}
				return true
			})
		case <-pr.ctx.Done():
			return
		}
	}
}

func (pr *perfRunner) markTestInFlight(tc TestCase, trackingID string) {
	mutex.Lock()
	defer mutex.Unlock()
	if len(trackingID) > 0 {
		pr.msgTimeMap.Store(trackingID, &inflightTest{
			testCase: tc,
			time:     time.Now(),
		})
	}
}

func (pr *perfRunner) recordCompletedAction() {
	if pr.ramping() {
		_ = atomic.AddInt64(&pr.summary.rampSummary, 1) // increment atomically
	} else {
		pr.summary.totalSummary++
		_ = atomic.AddInt64(&pr.summary.totalSummary, 1) // increment atomically
	}
}

func (pr *perfRunner) stopTrackingRequest(trackingID string) {
	log.Debugf("Deleting tracking request: %s", trackingID)
	pr.msgTimeMap.Delete(trackingID)
}

func (pr *perfRunner) IsDaemon() bool {
	return pr.daemon
}

func (pr *perfRunner) getIdempotencyKey(workerId int, iteration int) string {
	// Left pad worker ID to 5 digits (supporting up to 99,999 workers)
	workerIdStr := fmt.Sprintf("%05d", workerId)
	// Left pad iteration ID to 9 digits (supporting up to 999,999,999 iterations)
	iterationIdStr := fmt.Sprintf("%09d", iteration)
	return fmt.Sprintf("%v-%s-%s-%s", pr.startTime, workerIdStr, iterationIdStr, fftypes.NewUUID())
}

func (pr *perfRunner) calculateCurrentTps(logValue bool) float64 {
	// If we're still ramping, give the current rate during the ramp
	// If we're done ramping, calculate TPS from the end of the ramp onward
	var startTime int64
	var measuredActions int64
	if pr.ramping() {
		measuredActions = pr.summary.rampSummary
		startTime = pr.startRampTime
	} else {
		measuredActions = pr.summary.totalSummary
		startTime = pr.startTime
	}
	duration := time.Since(time.Unix(startTime, 0)).Seconds()
	currentTps := float64(measuredActions) / duration
	if logValue {
		log.Infof("Current TPS: %v Measured Actions: %v Duration: %v", currentTps, measuredActions, duration)
	}
	return currentTps
}

func (pr *perfRunner) ramping() bool {
	return time.Now().Before(time.Unix(pr.endRampTime, 0))
}
