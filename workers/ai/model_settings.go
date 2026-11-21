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
	// maxTokens is zero when the Agent sets no output limit, which leaves the
	// limit to each provider. Fallbacks can switch provider and model within a
	// Task, so the worker cannot choose one value for all of them.
	maxTokens int
}

func parseModelSettings(workerEnv workerenv.AIWorkerEnv) (modelSettings, error) {
	settings := modelSettings{}
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
		// A non-positive output cap counts as unset.
		if maxTokens > 0 {
			settings.maxTokens = int(maxTokens)
		}
	}
	return settings, nil
}
