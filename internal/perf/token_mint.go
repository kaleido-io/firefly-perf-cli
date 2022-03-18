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

	"github.com/hyperledger/firefly-perf-cli/internal/conf"
)

func (pr *perfRunner) RunTokenMint(nodeURL string, id int) {
	payload := fmt.Sprintf(`{
			"pool": "%s",
			"amount": "10",
			"to": "%s",
			"message": {
				"data": [
					{
						"value": "MintTokenPerformanceTest-%d"
					}
				],
				"header": {
					"tag": "%s"
				}
			}
		}`, pr.poolName, pr.cfg.RecipientAddress, id, fmt.Sprintf("%s_%d", pr.tagPrefix, id))
	req := pr.client.R().
		SetHeaders(map[string]string{
			"Accept":       "application/json",
			"Content-Type": "application/json",
		}).
		SetBody([]byte(payload))
	pr.sendAndWait(req, nodeURL, "tokens/mint", id, conf.PerfTestTokenMint.String())
}
