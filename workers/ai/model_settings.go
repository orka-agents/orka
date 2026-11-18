/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"math"
	"strconv"

	"github.com/orka-agents/orka/internal/workerenv"
)

type modelSettings struct {
	temperature    float64
	temperatureSet bool
	maxTokens      int
}

func parseModelSettings(workerEnv workerenv.AIWorkerEnv) (modelSettings, error) {
	settings := modelSettings{maxTokens: 4096}
	if workerEnv.Temperature != "" {
		temperature, err := strconv.ParseFloat(workerEnv.Temperature, 64)
		if err != nil || math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < 0 || temperature > 2 {
			// Parsing errors include the raw input, which must not enter worker logs or events.
			return modelSettings{}, fmt.Errorf("%s must be a finite number between 0 and 2", workerenv.AITemperature)
		}
		settings.temperature = temperature
		settings.temperatureSet = true
	}
	if workerEnv.MaxTokens != "" {
		maxTokens, err := strconv.ParseInt(workerEnv.MaxTokens, 10, 32)
		if err != nil {
			return modelSettings{}, fmt.Errorf("%s must be a signed 32-bit integer", workerenv.AIMaxTokens)
		}
		// Preserve the legacy default for omitted and non-positive Agent output caps.
		if maxTokens > 0 {
			settings.maxTokens = int(maxTokens)
		}
	}
	return settings, nil
}
