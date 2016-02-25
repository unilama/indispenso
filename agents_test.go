package main

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"testing"
)

type TestAgent struct {
	mock.Mock
}

func (t *TestAgent) AbortExecution(req *ConsensusRequest) error {
	args := t.Called(req)
	return args.Error(0)
}

func (t *TestAgent) Update(tags []string) error {
	args := t.Called(tags)
	return args.Error(0)
}
func (t *TestAgent) Id() string {
	args := t.Called()
	return args.String(0)
}
func (t *TestAgent) IsAlive() bool {
	args := t.Called()
	return args.Bool(0)
}
func (t *TestAgent) Commands() []Command {
	args := t.Called()
	return args.Get(0).([]Command)
}
func (t *TestAgent) HasTag(tag string) bool {
	args := t.Called(tag)
	return args.Bool(0)
}

/*
func TestAgentStore(t *testing.T) {

}
*/

func TestAgentStoreAbortExecution(t *testing.T) {
	agent1 := &TestAgent{}
	agent2 := &TestAgent{}

	req := newConsensusRequest()

	agent1.On("AbortExecution", mock.AnythingOfType("*main.ConsensusRequest")).Return(nil)
	agent2.On("AbortExecution", mock.AnythingOfType("*main.ConsensusRequest")).Return(nil)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": agent1, "test2": agent2}
	service.AbortConsensusExecution(req)

	agent1.AssertExpectations(t)
	agent2.AssertExpectations(t)
}

func TestAgentListBasedOnExclusionEmptyList(t *testing.T) {
	agent1 := &TestAgent{}

	agent1.On("HasTag", "a").Return(true)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": agent1}
	list, err := service.List([]string{}, []string{"a"})
	assert.Empty(t, list)
	assert.NoError(t, err)

	agent1.AssertExpectations(t)

}

func TestAgentListBasedOnExclusion(t *testing.T) {
	agent1 := &TestAgent{}
	agent2 := &TestAgent{}

	agent1.On("HasTag", "a").Return(false)
	agent2.On("HasTag", "a").Return(true)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": agent1, "test2": agent2}
	list, err := service.List([]string{}, []string{"a"})
	assert.NotEmpty(t, list)
	assert.Len(t, list, 1)
	assert.Equal(t, agent1, list[0])
	assert.NoError(t, err)

	agent1.AssertExpectations(t)

}

func TestAgentListBasedOnInclusionEmptyList(t *testing.T) {
	agent1 := &TestAgent{}

	agent1.On("HasTag", "a").Return(false)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": agent1}
	list, err := service.List([]string{"a"}, []string{})
	assert.Empty(t, list)
	assert.NoError(t, err)

	agent1.AssertExpectations(t)
}

func TestAgentListBasedOnInclusion(t *testing.T) {
	agent1 := &TestAgent{}
	agent2 := &TestAgent{}

	agent1.On("HasTag", "a").Return(false)
	agent2.On("HasTag", "a").Return(true)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": agent1, "test2": agent2}
	list, err := service.List([]string{"a"}, []string{})
	assert.NotEmpty(t, list)
	assert.Len(t, list, 1)
	assert.Equal(t, agent2, list[0])
	assert.NoError(t, err)

	agent1.AssertExpectations(t)
}

func TestAgentListBasedOnCriteria(t *testing.T) {
	agent1 := &TestAgent{}
	agent2 := &TestAgent{}
	agent3 := &TestAgent{}

	//no tags
	agent1.On("HasTag", "a").Return(false)
	agent1.On("HasTag", "b").Return(false)

	// both tags
	agent2.On("HasTag", "a").Return(true)
	agent2.On("HasTag", "b").Return(true)

	//only "a" tag
	agent3.On("HasTag", "a").Return(true)
	agent3.On("HasTag", "b").Return(false)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": agent1, "test2": agent2, "test3": agent3}
	list, err := service.List([]string{"a"}, []string{"b"})
	assert.NotEmpty(t, list)
	assert.Len(t, list, 1)
	assert.Equal(t, agent3, list[0])
	assert.NoError(t, err)

	agent1.AssertExpectations(t)
}

func TestAgentListAdd(t *testing.T) {
	service := newAgentStore()
	x := &TestAgent{}
	x.On("Id").Return("test1")
	err := service.Add(x)

	assert.NoError(t, err)
	assert.NotEmpty(t, service.agents)
	assert.Len(t, service.agents, 1)
	assert.Contains(t, service.agents, "test1")
	assert.Equal(t, x, service.agents["test1"])
}

func TestAgentListRemove(t *testing.T) {
	x := &TestAgent{}
	x.On("Id").Return("test1")

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": x}

	err := service.Remove(x)

	assert.NoError(t, err)
	assert.Empty(t, service.agents)
}

func TestAgentListRemoveNotExisting(t *testing.T) {
	x := &TestAgent{}
	x.On("Id").Return("test1")

	service := newAgentStore()

	err := service.Remove(x)

	assert.NoError(t, err)
	assert.Empty(t, service.agents)
}

func TestAgentListRemoveById(t *testing.T) {
	x := &TestAgent{}
	x.On("Id").Return("test1")

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": x}

	err := service.RemoveById("test1")

	assert.NoError(t, err)
	assert.Empty(t, service.agents)
}

func TestAgentListRemoveNotExistingById(t *testing.T) {
	x := &TestAgent{}
	x.On("Id").Return("test1")

	service := newAgentStore()

	err := service.RemoveById("test1")

	assert.NoError(t, err)
	assert.Empty(t, service.agents)
}

func TestAgentListGetNotExisting(t *testing.T) {
	service := newAgentStore()
	agent, err := service.Get("test")

	assert.Error(t, err)
	assert.Nil(t, agent)
}

func TestAgentListGet(t *testing.T) {
	x := &TestAgent{}
	x.On("Id").Return("test1")

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": x}

	agent, err := service.Get("test1")

	assert.NoError(t, err)
	assert.NotNil(t, agent)
	assert.Equal(t, x, agent)
}

func TestAgentListEmptyCleanup(t *testing.T) {
	service := newAgentStore()
	err := service.Cleanup()
	assert.NoError(t, err)
	assert.Empty(t, service.agents)
}

func TestAgentListAllAliveCleanup(t *testing.T) {
	x := &TestAgent{}
	//x.On("Id").Return("test1")
	x.On("IsAlive").Return(true)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": x}

	err := service.Cleanup()
	assert.NoError(t, err)
	assert.NotEmpty(t, service.agents)
	assert.Contains(t, service.agents, "test1")
}

func TestAgentListNotAliveCleanup(t *testing.T) {
	x := &TestAgent{}
	x.On("Id").Return("test1")
	x.On("IsAlive").Return(false)

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": x}

	err := service.Cleanup()
	assert.NoError(t, err)
	assert.Empty(t, service.agents)
}

func TestAgentListCommandsNoAgents(t *testing.T) {
	service := newAgentStore()
	list := service.ListCommands()
	assert.Empty(t, list)
}

func TestAgentListCommandsEmptyAgentCommands(t *testing.T) {
	x := &TestAgent{}
	x.On("Commands").Return([]Command{})

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": x}

	list := service.ListCommands()
	assert.NotEmpty(t, list)
	assert.Contains(t, list, "test1")
	assert.Empty(t, list["test1"])
}

func TestAgentListCommands(t *testing.T) {
	x := &TestAgent{}
	x.On("Commands").Return([]Command{nil})

	service := newAgentStore()
	service.agents = map[string]Agent{"test1": x}

	list := service.ListCommands()
	assert.NotEmpty(t, list)
	assert.Contains(t, list, "test1")
	assert.NotEmpty(t, list["test1"])
}
