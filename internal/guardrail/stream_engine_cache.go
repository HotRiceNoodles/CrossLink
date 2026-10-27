package guardrail

import "encoding/json"

// Stream engines are cached separately from the legacy Check cache so a policy
// reload cannot close a client still used by a live streaming session. Expensive
// rule compilation is shared; mutable StreamSession state is never shared.
type streamEngineKey struct {
	id, version  int64
	kind, config string
}
type streamEngineEntry struct {
	engine  GuardrailEngine
	users   int
	retired bool
}

func streamKey(rule GuardrailRule) streamEngineKey {
	return streamEngineKey{rule.ID, rule.Version, rule.Type, string(rule.Config)}
}

func (s *GuardrailService) leaseStreamEngine(rule GuardrailRule) (GuardrailEngine, func(), error) {
	s.streamEngineMu.Lock()
	defer s.streamEngineMu.Unlock()
	if s.streamEngines == nil {
		s.streamEngines = map[streamEngineKey]*streamEngineEntry{}
	}
	key := streamKey(rule)
	entry := s.streamEngines[key]
	if entry == nil {
		engine, err := CreateEngine(rule.Type, json.RawMessage(rule.Config), s.deps)
		if err != nil {
			return nil, nil, err
		}
		entry = &streamEngineEntry{engine: engine}
		s.streamEngines[key] = entry
	}
	entry.users++
	return entry.engine, func() {
		s.streamEngineMu.Lock()
		defer s.streamEngineMu.Unlock()
		entry.users--
		if entry.users == 0 && entry.retired {
			s.closeStreamEngine(key, entry)
		}
	}, nil
}

func (s *GuardrailService) closeStreamEngine(key streamEngineKey, entry *streamEngineEntry) {
	if closer, ok := entry.engine.(EngineCloser); ok {
		closer.Close()
	}
	delete(s.streamEngines, key)
}

func (s *GuardrailService) pruneStreamEngines(rules []GuardrailRule) {
	active := map[streamEngineKey]bool{}
	for _, rule := range rules {
		active[streamKey(rule)] = true
	}
	s.streamEngineMu.Lock()
	defer s.streamEngineMu.Unlock()
	for key, entry := range s.streamEngines {
		if !active[key] {
			entry.retired = true
			if entry.users == 0 {
				s.closeStreamEngine(key, entry)
			}
		}
	}
}
