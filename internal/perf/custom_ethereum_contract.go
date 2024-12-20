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
	"fmt"
	"strconv"
	"time"

	"github.com/hyperledger/firefly-perf-cli/internal/conf"
	log "github.com/sirupsen/logrus"

	"github.com/hyperledger/firefly-common/pkg/fftypes"
)

type customEthereum struct {
	testBase
}

func newCustomEthereumTestWorker(pr *perfRunner, workerID int, actionsPerLoop int) TestCase {
	return &customEthereum{
		testBase: testBase{
			pr:             pr,
			workerID:       workerID,
			actionsPerLoop: actionsPerLoop,
		},
	}
}

func (tc *customEthereum) Name() string {
	return conf.PerfTestCustomEthereumContract.String()
}

func (tc *customEthereum) IDType() TrackingIDType {
	return TrackingIDTypeWorkerNumber
}

type submitResponse struct {
	Result string `json:"result"`
}

func (tc *customEthereum) RunOnce(iterationCount int) (string, error) {
	idempotencyKey := tc.pr.getIdempotencyKey(tc.workerID, iterationCount)
	// TODO AM: the abi reference should come from the config too
	payload := fmt.Sprintf(`{
		"jsonrpc": "2.0",
		"id": "1",
		"method": "ptx_sendTransaction",
		"params": [
			{
				"type": "public",
				"abiReference": "0x23dbc09b901a3bf265a44b60ca7337eeba63f506ddd8ed77ac1505a52a2c5d15",
				"function": "set",
				"to": "%s",
				"from": "anna@node1",
				"data": [%v],
				"idempotencyKey": "%s"
			}
		]
	}`, tc.pr.cfg.ContractOptions.Address, tc.workerID, idempotencyKey)
	var resContractCall submitResponse
	var resError fftypes.RESTError
	res, err := tc.pr.client.R().
		SetHeaders(map[string]string{
			"Accept":       "application/json",
			"Content-Type": "application/json",
		}).
		SetBody([]byte(payload)).
		SetResult(&resContractCall).
		SetError(&resError).
		Post(tc.pr.client.BaseURL)
	if err != nil || res.IsError() {
		if res.StatusCode() == 409 {
			log.Warnf("Request already received by Paladin: %+v", &resError)
		} else {
			return "", fmt.Errorf("Error invoking contract [%d]: %s (%+v)", resStatus(res), err, &resError)
		}
	}
	tc.pr.txIDMap.Store(resContractCall.Result, time.Now())
	return strconv.Itoa(tc.workerID), nil
}
