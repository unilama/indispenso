package main

import (
	"fmt"
	"sync"
)

type Agent interface {
	AbortExecution(*ConsensusRequest) error
	Update([]string) error
	Id() string
	IsAlive() bool
	Commands() []Command
	HasTag(string) bool
}

type Command interface {
	IsExecution(*ExecutionCoordinatorEntry) bool
	GetId() string
	State() string
}

type AgentService interface {
	Add(Agent) error
	Remove(Agent) error
	RemoveById(string) error
	Get(string) (Agent, error)
	Cleanup() error
	ListCommands() map[string][]Command
	List([]string, []string) ([]Agent, error)
	AbortConsensusExecution(*ConsensusRequest) error
}

type AgentStore struct {
	agents    map[string]Agent
	agentsMux sync.RWMutex
}

func newAgentStore() *AgentStore {
	return &AgentStore{agents: map[string]Agent{}}
}

func (a *AgentStore) Add(agent Agent) error {
	a.agentsMux.Lock()
	defer a.agentsMux.Unlock()
	a.agents[agent.Id()] = agent
	return nil
}
func (a *AgentStore) Remove(agent Agent) error {
	a.RemoveById(agent.Id())
	return nil
}
func (a *AgentStore) RemoveById(Id string) error {
	a.agentsMux.Lock()
	defer a.agentsMux.Unlock()
	delete(a.agents, Id)
	return nil
}

func (a *AgentStore) Get(Id string) (Agent, error) {
	a.agentsMux.RLock()
	defer a.agentsMux.RUnlock()
	agent, ok := a.agents[Id]
	if ok {
		return agent, nil
	}
	return nil, fmt.Errorf("Cannot find agent with it %s", Id)
}
func (a *AgentStore) Cleanup() error {
	a.agentsMux.Lock()
	defer a.agentsMux.Unlock()
	for _, agent := range a.agents {
		if !agent.IsAlive() {
			// Disconnect
			log.Printf("Client %s disconnected", agent.Id())
			delete(a.agents, agent.Id())
		}
	}
	return nil
}
func (a *AgentStore) ListCommands() map[string][]Command {
	commands := make(map[string][]Command, len(a.agents))
	a.agentsMux.RLock()
	for id, agent := range a.agents {
		commands[id] = agent.Commands()
	}
	a.agentsMux.RUnlock()
	return commands
}
func (a *AgentStore) List(include []string, exclude []string) ([]Agent, error) {

	a.agentsMux.RLock()
	defer a.agentsMux.RUnlock()

	res := make([]Agent, 0, len(a.agents))
	for _, agent := range a.agents {
		// Excluded? One match is enough to skip this one
		excluded := false
		if len(exclude) > 0 {

			for _, tag := range exclude {
				excluded = agent.HasTag(tag)
				if excluded {
					break
				}
			}
		}

		if excluded {
			continue
		}

		// Included? Must have all
		var match bool = true
		for _, tag := range include {
			if !agent.HasTag(tag) {
				match = false
				break
			}
		}
		if len(include) > 0 && match == false {
			continue
		}
		res = append(res, agent)
	}

	return res, nil
}

func (a *AgentStore) AbortConsensusExecution(req *ConsensusRequest) error {
	a.agentsMux.RLock()
	for _, agent := range a.agents {
		agent.AbortExecution(req)
	}
	a.agentsMux.RUnlock()
	return nil
}
