package main

import (
	// "bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// @author Robin Verlangen

type Cmd struct {
	Command              string   // Commands to execute
	Pending              bool     // Did we dispatch it to the client?
	Id                   string   // Unique ID for this command
	ClientId             string   // Client ID on which the command is executed
	TemplateId           string   // Reference to the template id
	ConsensusRequestId   string   // Reference to the request id
	Signature            string   // makes this only valid from the server to the client based on the preshared token and this is a signature with the command and id
	Timeout              int      // in seconds
	state                string   // Textual representation of the current state, e.g. finished, failed, etc.
	RequestUserId        string   // User ID of the user that initiated this command
	Created              int64    // Unix timestamp created
	ExecutionIterationId int      // In which iteration the command was started
	BufOutput            []string // Standard output
	BufOutputErr         []string // Error output
}

// Sign the command on the server
func (c *Cmd) Sign(client *RegisteredClient) {
	c.Signature = c.ComputeHmac(client.AuthToken)
}

func (c *Cmd) GetId() string {
	return c.Id
}

func (c *Cmd) State() string {
	return c.state
}

func (c *Cmd) IsExecution(entry *ExecutionCoordinatorEntry) bool {
	return c.ConsensusRequestId == entry.Id && c.ExecutionIterationId == entry.iteration
}

// Set local state
func (c *Cmd) SetState(state string) {
	// Old state for change detection
	oldState := c.state

	// Update
	c.state = state

	// Debug logging
	if conf.Debug {
		log.Printf("Cmd %s went from state %s to %s", c.Id, oldState, c.state)
	}

	// Run validation
	if oldState == "finished_execution" && c.state == "flushed_logs" {
		c._validate()
	} else if oldState == "failed_execution" && c.state == "flushed_logs" {
		c.state = "failed"
	}
}

// Validate the execution of a command, only on the server
func (c *Cmd) _validate() {
	// Only on the server
	if !conf.ServerEnabled {
		return
	}

	// Get template
	template := server.templateStore.Get(c.TemplateId)
	if template == nil {
		log.Printf("Unable to find template %s for validation of cmd %s", c.TemplateId, c.GetId)
		return
	}

	// Iterate and run on templates
	var failedValidation = false
	for _, v := range template.ValidationRules {
		// Select stream
		var stream []string
		if v.OutputStream == 1 {
			stream = c.BufOutput
		} else {
			stream = c.BufOutputErr
		}

		// Match on line
		var matched bool = false
		for _, line := range stream {
			if strings.Contains(line, v.Text) {
				matched = true
				break
			}
		}

		// Did we match?
		if v.MustContain == true && matched == false {
			// Should BE there, but is NOT
			c.SetState("failed_validation")
			failedValidation = true
			break
		} else if v.MustContain == false && matched == true {
			// Should NOT be there, but IS
			c.SetState("failed_validation")
			failedValidation = true
			break
		}
	}

	// Done and passed validation
	if failedValidation == false {
		if conf.Debug {
			log.Printf("Validation passed for %s", c.GetId)
		}
		c.SetState("finished")

		// Start next iteration
		ece := server.executionCoordinator.Get(c.ConsensusRequestId)
		if ece != nil {
			go ece.Next()
		}
	}
}

// Notify state to server
func (c *Cmd) NotifyServer(state string) {
	// Update local client state
	c.SetState(state)

	// Update server state, only if this has a signature, else it is local
	if len(c.Signature) > 0 {
		client._req("PUT", fmt.Sprintf("client/%s/cmd/%s/state?state=%s", url.QueryEscape(client.Id), url.QueryEscape(c.GetId()), url.QueryEscape(state)), nil)
	}
}

// Should we flush the local buffer? After X milliseconds or Y lines
func (c *Cmd) _checkFlushLogs() {
	// At least 10 lines
	if len(c.BufOutput) > 10 || len(c.BufOutputErr) > 10 {
		c._flushLogs()
	}
}

// Write logs to server
func (c *Cmd) _flushLogs() {
	// Only if this has a signature, else it is local
	if len(c.Signature) < 1 {
		return
	}

	// To JSON
	m := make(map[string][]string)
	m["output"] = c.BufOutput
	m["error"] = c.BufOutputErr
	bytes, je := json.Marshal(m)
	if je != nil {
		log.Printf("Failed to convert logs to JSON: %s", je)
		return
	}

	// Post to server
	uri := fmt.Sprintf("client/%s/cmd/%s/logs", url.QueryEscape(client.Id), url.QueryEscape(c.GetId()))
	b, e := client._req("PUT", uri, bytes)
	if e != nil || len(b) < 1 {
		log.Printf("Failed log write: %s", e)
	}

	// Clear buffers
	c.BufOutput = make([]string, 0)
	c.BufOutputErr = make([]string, 0)
}

// Log output
func (c *Cmd) LogOutput(line string) {
	// No lock, only one routine can access this

	// Append
	c.BufOutput = append(c.BufOutput, line)

	// Check to flush?
	c._checkFlushLogs()
}

// Log error
func (c *Cmd) LogError(line string) {
	// No lock, only one routine can access this

	// Append
	c.BufOutputErr = append(c.BufOutputErr, line)

	// Check to flush?
	c._checkFlushLogs()
}

// Sign the command
func (c *Cmd) ComputeHmac(token string) string {
	bytes, be := base64.URLEncoding.DecodeString(token)
	if be != nil {
		return ""
	}
	mac := hmac.New(sha256.New, bytes)
	mac.Write([]byte(c.Command))
	mac.Write([]byte(c.GetId()))
	sum := mac.Sum(nil)
	return base64.URLEncoding.EncodeToString(sum)
}

// Execute command on the client
func (c *Cmd) Execute(client *Client) {
	log.Printf("Executing %s: %s", c.GetId, c.Command)

	// Validate HMAC
	c.NotifyServer("validating")
	if client != nil {
		// Compute mac
		expectedMac := c.ComputeHmac(client.AuthToken)

		// Valid?
		if expectedMac != c.Signature || len(c.Signature) < 1 {
			// No, let's abort

			// Notify server
			c.NotifyServer("invalid_signature")

			// Log
			log.Printf("ERROR! Invalid command signature, communication between server and client might be tampered with")

			// Re-authenticate with server in order to establish a new token
			client.AuthServer()

			// Abort execution
			return
		}
	} else {
		log.Printf("Executing insecure command, unable to validate HMAC of %s", c.GetId)
	}

	// Start
	c.NotifyServer("starting")

	// File contents
	var fileBytes bytes.Buffer
	fileBytes.WriteString("#!/bin/bash\n")
	fileBytes.WriteString(c.Command)

	// Write tmp file
	tmpFileName := fmt.Sprintf("/tmp/indispenso_%s", c.GetId)
	ioutil.WriteFile(tmpFileName, fileBytes.Bytes(), 0644)

	// Remove file once done
	defer os.Remove(tmpFileName)

	// Run file
	cmd := exec.Command("bash", tmpFileName)
	var out bytes.Buffer
	var outerr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &outerr

	// Consume streams
	// go func() {
	// 	p, pe := cmd.StdoutPipe()
	// 	if pe != nil {
	// 		log.Printf("Pipe error: %s", pe)
	// 		return
	// 	}
	// 	scanner := bufio.NewScanner(p)
	// 	for scanner.Scan() {
	// 		txt := scanner.Text()
	// 		c.LogOutput(txt)
	// 		if conf.Debug {
	// 			log.Println(scanner.Text())
	// 		}
	// 	}
	// 	if err := scanner.Err(); err != nil {
	// 		fmt.Fprintln(os.Stderr, "reading standard input:", err)
	// 	}
	// }()
	// go func() {
	// 	p, pe := cmd.StderrPipe()
	// 	if pe != nil {
	// 		log.Printf("Pipe error: %s", pe)
	// 		return
	// 	}
	// 	scanner := bufio.NewScanner(p)
	// 	for scanner.Scan() {
	// 		txt := scanner.Text()
	// 		c.LogError(txt)
	// 		if conf.Debug {
	// 			log.Println(scanner.Text())
	// 		}
	// 	}
	// 	if err := scanner.Err(); err != nil {
	// 		fmt.Fprintln(os.Stderr, "reading standard input:", err)
	// 	}
	// }()

	// Start
	err := cmd.Start()
	if err != nil {
		c.NotifyServer("failed_execution")
		log.Printf("Failed to start command: %s", err)
		return
	}
	c.NotifyServer("started_execution")

	// Timeout mechanism
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-time.After(time.Duration(c.Timeout) * time.Second):
		if err := cmd.Process.Kill(); err != nil {
			log.Printf("Failed to kill %s: %s", c.GetId, err)
			return
		}
		<-done // allow goroutine to exit
		c.NotifyServer("killed_execution")
		log.Printf("Process %s killed", c.GetId)
	case err := <-done:
		if err != nil {
			c.NotifyServer("failed_execution")
			c.LogError(fmt.Sprintf("%v", err))
			log.Printf("Process %s done with error = %v", c.GetId, err)
		} else {
			c.NotifyServer("finished_execution")
			log.Printf("Finished %s", c.GetId)
		}
	}

	// Logs
	for _, line := range strings.Split(out.String(), "\n") {
		c.LogOutput(line)
	}
	for _, line := range strings.Split(outerr.String(), "\n") {
		c.LogError(line)
	}
	// Final flush
	c._flushLogs()
	c.NotifyServer("flushed_logs")
}

func newCmd(command string, timeout int) *Cmd {
	// Default timeout if not valid
	if timeout < 1 {
		timeout = DEFAULT_COMMAND_TIMEOUT
	}

	// Create instance
	return &Cmd{
		Id:           uuidStr(),
		Command:      command,
		Pending:      true,
		Timeout:      timeout,
		state:        "pending",
		Created:      time.Now().Unix(),
		BufOutput:    make([]string, 0),
		BufOutputErr: make([]string, 0),
	}
}
