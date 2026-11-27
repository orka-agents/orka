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
	settings := modelSettings{}
	if workerEnv.Temperature != "" {
		temperature, err := strconv.ParseFloat(workerEnv.Temperature, 64)
		if err != nil || math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < 0 || temperature > 2 {
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
		if maxTokens > 0 {
			settings.maxTokens = int(maxTokens)
		}
	}
	return settings, nil
}
