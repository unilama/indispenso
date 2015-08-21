package main

import (
	"net/http"
	"fmt"
	"time"
	"github.com/julienschmidt/httprouter"
	"github.com/antonholmquist/jason"
	"io/ioutil"
	"math"
	"math/rand"
	"net/url"
)

// Client methods (one per "slave", communicates with the server)

type Client struct {

}

// Start client
func (s *Client) Start() bool {
	log.Println("Starting client")

	// Start webserver
	go func() {
		router := httprouter.New()
	    router.GET("/ping", Ping)

	    log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", clientPort), router))
    }()

    // Register with server
    go func() {
    	go func() {
	    	s.PingServer()
    	}()
	    c := time.Tick(time.Duration(CLIENT_PING_INTERVAL) * time.Second)
	    for _ = range c {
	    	s.PingServer()
	    }
    }()

    // Long poll commands
    go func() {
    	for {
    		s.PollCmds()
    	}
    }()

	return true
}

// Fetch commands
func (s *Client) PollCmds() {
	bytes, err := s._get(fmt.Sprintf("client/%s/cmds", url.QueryEscape(hostname)))
	if err == nil {
		log.Println(string(bytes))
		obj, jerr := jason.NewObjectFromBytes(bytes)
		if jerr == nil {
			cmds, _ := obj.GetObjectArray("cmds")
			for _, cmd := range cmds {
				id, _ := cmd.GetString("Id")
				command, _ := cmd.GetString("Command")
				timeout, _ := cmd.GetInt64("Timeout")
				cmd := newCmd(command, int(timeout))
				cmd.Id = id
				cmd.Execute()
			}
		}
	}
}

// Ping server
func (s *Client) PingServer() {
	s._get(fmt.Sprintf("client/%s/ping", url.QueryEscape(hostname)))
}

// Get
func (s *Client) _get(uri string) ([]byte, error) {
	return s._req("GET", uri, nil)
}

// Generic request method with retry handling
func (s *Client) _req(method string, uri string, data []byte) ([]byte, error) {
	var bytes []byte = nil
	var err error = nil
	for i := 0; i < 10; i++ {
		bytes, err = s._reqUnsafe(method, uri, data)
		if err == nil {
			return bytes, err
		}

		// Sleep a bit before the retry and apply ~25ms jitter
		var sleep float64 = 25 + float64(rand.Intn(50)) + (math.Pow(float64(i), 2) * 10000)
		time.Sleep(time.Duration(sleep) * time.Millisecond)
	}
	return bytes, err
}

// Generic request method
func (s *Client) _reqUnsafe(method string, uri string, data []byte) ([]byte, error) {
	// Client
	client := &http.Client{}

	// Req
	// @todo support data
	req, reqErr := http.NewRequest(method, fmt.Sprintf("%s%s", seedUri, uri), nil)
	if reqErr != nil {
		return nil, reqErr
	}

	// Auth token
	req.Header.Add("X-Auth", secureToken)

	// Execute
	resp, respErr := client.Do(req)
	if respErr != nil {
		return nil, respErr
	}

	// Read body
	body, bodyErr := ioutil.ReadAll(resp.Body)
	if bodyErr != nil {
		return nil, bodyErr
	}
	return body, nil
}

// Create new client
func newClient() *Client {
	return &Client{}
}