package guardrail

import (
	"encoding/json"
	"fmt"
	"sync"
)

type EngineFactory func(config json.RawMessage) (GuardrailEngine, error)

type EngineFactoryV2 func(config json.RawMessage, deps EngineDeps) (GuardrailEngine, error)

var (
	engineFactories = map[string]interface{}{}
	registryMu      sync.RWMutex
)

func RegisterEngine(ruleType string, factory EngineFactory) {
	registryMu.Lock()
	engineFactories[ruleType] = factory
	registryMu.Unlock()
}

func RegisterEngineV2(ruleType string, factory EngineFactoryV2) {
	registryMu.Lock()
	engineFactories[ruleType] = factory
	registryMu.Unlock()
}

func CreateEngine(ruleType string, config json.RawMessage, deps ...EngineDeps) (GuardrailEngine, error) {
	registryMu.RLock()
	factory, ok := engineFactories[ruleType]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown guardrail engine type: %s", ruleType)
	}
	switch f := factory.(type) {
	case EngineFactory:
		return f(config)
	case EngineFactoryV2:
		var d EngineDeps
		if len(deps) > 0 {
			d = deps[0]
		}
		return f(config, d)
	default:
		return nil, fmt.Errorf("unknown factory type for engine: %s", ruleType)
	}
}

func RegisteredTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	types := make([]string, 0, len(engineFactories))
	for t := range engineFactories {
		types = append(types, t)
	}
	return types
}
