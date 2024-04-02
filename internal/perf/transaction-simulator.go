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
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"

	"github.com/hyperledger/firefly-perf-cli/internal/conf"
	log "github.com/sirupsen/logrus"

	"github.com/hyperledger/firefly-common/pkg/fftypes"
)

type transactionSimulator struct {
	testBase
	transactionSimulatorConfig *TransactionSimulatorConfig
}

type TransactionSimulatorOptions struct {
	PartiesCount    int    `yaml:"partiesCount" json:"partiesCount"`
	PartyNamePrefix string `yaml:"partyNamePrefix,omitempty" json:"partyNamePrefix,omitempty"`
	MinAmount       *int   `yaml:"minAmount,omitempty" json:"minAmount,omitempty"`
	MaxAmount       *int   `yaml:"maxAmount,omitempty" json:"maxAmount,omitempty"`
	DryRun          bool   `yaml:"dryRun" json:"dryRun"`
}

type TransactionSimulatorConfig struct {
	From      string
	To        string
	MinAmount int
	MaxAmount int
	DryRun    bool // purely for testing purpose
}

func newTransactionSimulatorWorker(pr *perfRunner, workerID int, actionsPerLoop int, tsc *TransactionSimulatorConfig) TestCase {
	return &transactionSimulator{
		testBase: testBase{
			pr:             pr,
			workerID:       workerID,
			actionsPerLoop: actionsPerLoop,
		},
		transactionSimulatorConfig: tsc,
	}
}

func (tc *transactionSimulator) Name() string {
	return conf.PerfTestCustomEthereumContract.String()
}

func (tc *transactionSimulator) IDType() TrackingIDType {
	return TrackingIDTypeWorkerNumber
}

func (tc *transactionSimulator) RunOnce(iterationCount int) (string, error) {
	idempotencyKey := tc.pr.getIdempotencyKey(tc.workerID, iterationCount)
	invokeOptionsJSON := ""
	if tc.pr.cfg.InvokeOptions != nil {
		b, err := json.Marshal(tc.pr.cfg.InvokeOptions)
		if err == nil {
			invokeOptionsJSON = fmt.Sprintf(",\n		 \"options\": %s", b)
		}
	}

	payload := fmt.Sprintf(`{
		"from": "%s",
		"to": "%s",
		"value": %d,
		"idempotencyKey": "%s"%s
	}`, tc.transactionSimulatorConfig.From, tc.transactionSimulatorConfig.To, rand.Intn(tc.transactionSimulatorConfig.MaxAmount-tc.transactionSimulatorConfig.MinAmount+1)+tc.transactionSimulatorConfig.MinAmount, idempotencyKey, invokeOptionsJSON)
	var resContractCall map[string]interface{}
	var resError fftypes.RESTError
	fullPath, err := url.JoinPath(tc.pr.client.BaseURL, "transfer")
	if err != nil {
		return "", err
	}
	if tc.transactionSimulatorConfig.DryRun {
		log.Info("DRYRUN: posting to %s, with payload %s", fullPath, payload)

	} else {

		res, err := tc.pr.client.R().
			SetHeaders(map[string]string{
				"Accept":       "application/json",
				"Content-Type": "application/json",
			}).
			SetBody([]byte(payload)).
			SetResult(&resContractCall).
			SetError(&resError).
			Post(fullPath)
		if err != nil || res.IsError() {
			if res.StatusCode() == 409 {
				log.Warnf("Request already received by the endpoint: %+v", &resError)
			} else {
				return "", fmt.Errorf("error submitting transaction [%d]: %s (%+v)", resStatus(res), err, &resError)
			}
		}
	}
	return strconv.Itoa(tc.workerID), nil
}
