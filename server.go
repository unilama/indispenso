package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/RobinUS2/golang-jresp"
	"github.com/julienschmidt/httprouter"
	"github.com/nu7hatch/gouuid"
	"github.com/spf13/cast"
	"github.com/unilama/indispenso/data_table"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server methods (you probably only need one or two in HA failover mode)

type Server struct {
	agentService AgentService

	Tags    map[string]bool
	tagsMux sync.RWMutex

	userStore            *UserStore
	templateStore        *TemplateStore
	consensus            *Consensus
	executionCoordinator *ExecutionCoordinator
	httpCheckStore       *HttpCheckStore
	authService          *AuthService

	InstanceId string // Unique ID generated at startup of the server, used for re-authentication and client-side refresh after and update/restart
}

// Register client
func (s *Server) RegisterClient(clientId string, tags []string) {

	agent, _ := s.agentService.Get(clientId)
	if agent == nil {
		agent = newRegisteredClient(clientId)
		s.agentService.Add(agent)
		log.Printf("Client %s registered with tags %s", clientId, tags)
	}

	agent.Update(tags)

	// Update tags
	s.tagsMux.Lock()
	for _, tag := range tags {
		s.Tags[tag] = true
	}
	s.tagsMux.Unlock()
}

// TODO consider to remove
func (s *Server) GetClient(clientId string) *RegisteredClient {
	client, _ := s.agentService.Get(clientId)

	//TODO unsafe
	return client.(*RegisteredClient)
}

// Scan for old clients
func (s *Server) CleanupClients() {

}

// Submit command to registered client using channel notify system
func (client *RegisteredClient) Submit(cmd *Cmd) {
	client.mux.Lock()

	// Command in pending list, this will be polled of within milliseconds
	client.Cmds[cmd.GetId()] = cmd

	// Keep track of command status
	client.DispatchedCmds[cmd.GetId()] = cmd

	client.mux.Unlock()

	// Log
	audit.Log(nil, "Execute", fmt.Sprintf("Command '%s' on client %s with id %s", cmd.Command, client.ClientId, cmd.GetId()))

	// Signal for work
	client.CmdChan <- true
}

// A client that is registered with the server
type RegisteredClient struct {
	mux       sync.RWMutex
	ClientId  string
	AuthToken string `json:"-"` // Do not add to JSON
	LastPing  time.Time
	Tags      []string

	// Dispatched commands to the client
	DispatchedCmds map[string]*Cmd

	// Pending commands
	Cmds map[string]*Cmd

	// Channel used to trigger the long poll to fire a command to the client
	CmdChan chan bool `json:"-"`
}

func (c *RegisteredClient) Commands() []Command {
	commands := make([]Command, len(c.DispatchedCmds))
	i := 0

	c.mux.RLock()
	defer c.mux.RUnlock()
	for _, cmd := range c.DispatchedCmds {
		commands[i] = cmd
		i++
	}
	return commands
}

func (c *RegisteredClient) Id() string {
	return c.ClientId
}

// Get list of dispatched commands
// will automatically purge commands older than X days
func (c *RegisteredClient) GetDispatchedCmds() map[string]*Cmd {
	// Max age
	maxAge := time.Now().Unix() - (14 * 86400)

	// Is this one dirty? Meaning it contains too old data?
	dirty := false

	// Placeholder of list
	newMap := make(map[string]*Cmd, 0)

	// Build list
	c.mux.RLock()
	for k, d := range c.DispatchedCmds {
		if d.Created >= maxAge {
			newMap[k] = d
		} else {
			dirty = true
		}
	}
	c.mux.RUnlock()

	// Swap?
	if dirty {
		c.mux.Lock()
		if conf.Debug {
			log.Printf("Cleaning up dispatched commands of client %s size went from %d to %d", c.ClientId, len(c.DispatchedCmds), len(newMap))
		}
		c.DispatchedCmds = newMap
		c.mux.Unlock()
	}

	return newMap
}

func (c *RegisteredClient) AbortExecution(req *ConsensusRequest) error {
	c.mux.Lock()
	for k, cmd := range c.DispatchedCmds {
		if cmd.ConsensusRequestId == req.Id {
			delete(c.DispatchedCmds, k)
		}
	}
	c.mux.Unlock()
	return nil
}

func (c *RegisteredClient) Update(tags []string) error {
	c.mux.Lock()
	defer c.mux.Unlock()
	c.LastPing = time.Now()
	c.Tags = tags
	return nil
}

// Does this register client have this tag?
func (c *RegisteredClient) HasTag(s string) bool {
	if c.Tags == nil {
		return false
	}
	if len(c.Tags) == 0 {
		return false
	}
	for _, tag := range c.Tags {
		if tag == s {
			return true
		}
	}
	return false
}

func (c *RegisteredClient) IsAlive() bool {
	return time.Now().Sub(c.LastPing).Seconds() > float64(CLIENT_PING_INTERVAL*5)
}

// Generate keys
func (s *Server) _prepareTlsKeys() error {
	if _, err := os.Stat(conf.GetSslCertFile()); os.IsNotExist(err) {
		if !conf.AutoGenerateCert {
			log.Printf("Cannot locat certificate file(%s) doesn't exist, provide one or enable automatic self signed cert generation", conf.GetSslCertFile())
			return err
		}
		privateKey, err := _readOrGeneratePrivateKey(conf.GetSslPrivateKeyFile())
		if err != nil {
			return err
		}
		return _generateCertificate(conf.GetSslCertFile(), privateKey, 365*24*time.Hour)
	}
	return nil
}

func _readOrGeneratePrivateKey(fileName string) (*rsa.PrivateKey, error) {
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)
		keyOut, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		defer keyOut.Close()
		if err != nil {
			log.Printf("failed to open %s for writing: %s", fileName, err)
			return privateKey, err
		}
		pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
		return privateKey, nil
	} else {
		log.Printf("Private key (%s) exists and will be used", fileName)
		certArray, err := ioutil.ReadFile(fileName)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(certArray)
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	}
}

/**
Function that provide Serial number for certificate.

The serial number is chosen by the CA which issued the certificate.
It is just written in the certificate. The CA can choose the serial
number in any way as it sees fit.
*/
func _getCertSerialNumber() *big.Int {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Printf("failed to generate serial number: %s", err)
	}
	return serialNumber
}

func _generateCertificateTmpl(subject pkix.Name, validPeriod time.Duration) x509.Certificate {
	currentTime := time.Now()
	return x509.Certificate{
		SerialNumber: _getCertSerialNumber(),
		Subject:      subject,

		NotBefore: currentTime,
		NotAfter:  currentTime.Add(validPeriod),

		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        false,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1).To4()},
	}
}

func _generateCertificate(fileName string, privateKey *rsa.PrivateKey, validPeriod time.Duration) error {

	log.Println("Autogeneration of selfsigned certificate key...")
	subject := pkix.Name{
		Organization:       []string{"Indispenso"},
		Country:            []string{"NL"},
		CommonName:         "ssl.indispenso.org",
		OrganizationalUnit: []string{"IT"},
		Province:           []string{"Indispenso"},
		Locality:           []string{"Indispenso"},
	}

	tmpl := _generateCertificateTmpl(subject, validPeriod)
	certBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &privateKey.PublicKey, privateKey)
	if err != nil {
		log.Printf("Failed to create certificate: %s\n", err)
		return err
	}
	log.Println("Public key generated sucessfully")

	certFile, err := os.Create(fileName)
	if err != nil {
		log.Printf("Cannot create cert file : %s\n", err)
		return err
	}

	pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
	certFile.Close()
	log.Printf("Successfully written certificate: %s\n", fileName)
	return err
}

// Start server
func (s *Server) Start() bool {
	// Users
	s.userStore = newUserStore(conf.HomeFile("users.json"))

	s.authService = createAuthService(s.userStore)

	// Templates
	s.templateStore = newTemplateStore()

	// Consensus handler
	s.consensus = newConsensus()

	// Coordinator
	s.executionCoordinator = newExecutionCoordinator()

	// HTTP checks
	s.httpCheckStore = newHttpCheckStore()

	// Print info
	log.Printf("Starting server at https://localhost:%d/", conf.ServerPort)

	// Start webserver
	go func() {
		router := httprouter.New()

		// Homepage that redirects to /console
		router.GET("/", Home)

		// For uptime checks
		router.GET("/ping", Ping)

		// List tags
		router.GET("/tags", GetTags)

		// Client commands
		router.GET("/client/:clientId/ping", ClientPing)
		router.GET("/client/:clientId/cmds", ClientCmds)
		router.PUT("/client/:clientId/cmd/:cmd/state", PutClientCmdState)
		router.PUT("/client/:clientId/cmd/:cmd/logs", PutClientCmdLogs)
		router.GET("/client/:clientId/cmd/:cmd/logs", GetClientCmdLogs)
		router.POST("/client/:clientId/auth", PostClientAuth)

		// Auth endpoint
		router.POST("/auth", PostAuth)

		// Templates
		router.GET("/templates", GetTemplate)
		router.POST("/template/:templateid/validation", PostTemplateValidation)
		router.DELETE("/template/:templateid/validation/:id", DeleteTemplateValidation)
		router.POST("/template", PostTemplate)
		router.DELETE("/template", DeleteTemplate)

		// Update password
		router.PUT("/user/password", PutUserPassword)

		// List clients (~ slaves)
		router.GET("/clients", GetClients)

		// List users
		router.GET("/users", GetUsers)

		// List user names by ids
		router.GET("/users/names", GetUsersNames)

		// Create user
		router.POST("/user", PostUser)

		// Remove user
		router.DELETE("/user", DeleteUser)

		//Change user
		router.PUT("/user", PutUser)

		// Consensus requests
		router.POST("/consensus/request", PostConsensusRequest)
		router.DELETE("/consensus/request", DeleteConsensusRequest)
		router.POST("/consensus/approve", PostConsensusApprove)
		router.GET("/consensus/pending", GetConsensusPending)

		// Dispatched commands list
		router.POST("/dispatched", data_table.DefaultStoreHandler(DispatchedCmdQuery))

		// Http checks
		router.GET("/http-check/:id", GetHttpCheck)
		router.GET("/http-checks", GetHttpChecks)
		router.POST("/http-check", PostHttpCheck)
		router.DELETE("/http-check", DeleteHttpCheck)

		// Two factor auth
		router.GET("/user/2fa", GetUser2fa)
		router.PUT("/user/2fa", PutUser2fa)

		// Backup
		router.GET("/backup/configs.zip", GetBackupConfigs)

		// Console endpoint for interface
		router.ServeFiles("/console/*filepath", http.Dir("console"))

		// Auto generate key
		if err := s._prepareTlsKeys(); err != nil {
			log.Printf("TLS preperation failed due to : %s", err)
			log.Fatal("Unable to start server")
		}

		// Start server
		log.Printf("Failed to start server %v", http.ListenAndServeTLS(fmt.Sprintf(":%d", conf.ServerPort), conf.GetSslCertFile(), conf.GetSslPrivateKeyFile(), router))
	}()

	// Minutely cleanups etc
	go func() {
		c := time.Tick(1 * time.Minute)
		for _ = range c {
			server.agentService.Cleanup()
		}
	}()

	return true
}

func createAuthService(us *UserStore) *AuthService {
	as := newAuthService(us, DefaultFirstFactorAuth, newGAuthAuthenticator())

	if conf.EnableLdap {
		as.appendFirstFactor(newLdapAuthenticator(conf.ldapConfig, us))
	}

	return as
}

// Get logs from dispatched job
func GetClientCmdLogs(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetClientCmdLogs ")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get client
	registeredClient := server.GetClient(ps.ByName("clientId"))
	if registeredClient == nil {
		jr.Error("Client not registered")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Command
	cmdId := ps.ByName("cmd")
	registeredClient.mux.RLock()
	cmd := registeredClient.DispatchedCmds[cmdId]
	registeredClient.mux.RUnlock()
	if cmd == nil {
		jr.Error("Command not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	jr.Set("log_output", cmd.BufOutput)
	jr.Set("log_error", cmd.BufOutputErr)

	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Enable user two factor
func PutUser2fa(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for PutUser2fa")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// User
	user := getUser(r)
	if len(user.TotpSecret) < 1 {
		jr.Error("Two factor authentication not setup")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	if user.HasTwoFactor() {
		jr.Error("Two factor authentication already setup")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Gather values
	value1 := r.PostFormValue("token_1")
	value2 := r.PostFormValue("token_2")
	if value1 == value2 || strings.TrimSpace(value1) == strings.TrimSpace(value2) {
		jr.Error("The two values should not be the same. Wait for the next token (in a few seconds) to show up and provide that.")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Validate
	valid1, _ := user.ValidateTotp(value1)
	valid2, _ := user.ValidateTotp(value2)
	res := valid1 && valid2 // Both must match
	if res == false {
		jr.Error("The two tokens do not match. Make sure that the clock is set correctly on your mobile device and the Indispenso server.")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Enable
	if res {
		user.TotpSecretValidated = true
		user.AuthType |= AUTH_TYPE_TWO_FACTOR
		server.userStore.save()
	}

	jr.Set("enabled", res)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Get user two factor data
func GetUser2fa(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetUser2fa")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// User
	user := getUser(r)
	if user.HasTwoFactor() {
		jr.Error("Two factor authentication already setup")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	if err := user.GenerateOTPSecret(); err != nil {
		jr.Error(err.Error())
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	server.userStore.save()

	qrImageBytes, err := user.TotpQrImage()
	if err != nil {
		jr.Error(err.Error())
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	jr.Set("Secret", user.TotpSecret)
	jr.Set("Png", qrImageBytes)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

func DispatchedCmdQuery(tableStore *data_table.DefaultStore) *data_table.DefaultStore {
	// Fetch and create
	for clientId, cmds := range server.agentService.ListCommands() {
		for _, d := range cmds {
			//TODO unsafe
			cmd := d.(*Cmd)
			commandTime := time.Unix(cmd.Created, 0)
			row := make(map[string]interface{})
			row["created"] = commandTime.Format("2006-01-02 15:04:05")

			template := server.templateStore.Get(cmd.TemplateId)
			if template != nil {
				row["template"] = template.Title
			} else {
				row["template"] = "-"
			}

			user := server.userStore.ById(cmd.RequestUserId)
			if user != nil {
				row["user"] = user.Username
			} else {
				row["user"] = "-"
			}

			row["client"] = clientId
			row["state"] = cmd.State()
			row["link"] = fmt.Sprintf("logs?id=%s&client=%s", d.GetId(), clientId)
			rowObj := tableStore.CreateRow(row)
			if time.Since(commandTime).Hours() > 24 {
				rowObj.RowClass = "history-old"
			}
			tableStore.AddRow(rowObj)
		}
	}

	return tableStore
}

// Get pending execution request
func GetConsensusPending(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetConsensusPending")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	user := getUser(r)

	server.consensus.pendingMux.RLock()
	pending := make([]*ConsensusRequest, 0)
	work := make([]*ConsensusRequest, 0)
	for _, req := range server.consensus.Pending {
		// Ignore already executed
		if req.Executed {
			continue
		}

		// Ignore self
		if req.RequestUserId == user.Id {
			pending = append(pending, req)
			continue
		}

		// Voted?
		if req.ApproveUserIds[user.Id] == true {
			pending = append(pending, req)
			continue
		}

		work = append(work, req)
	}
	jr.Set("requests", pending)
	jr.Set("server_instance_id", server.InstanceId)
	jr.Set("work", work)
	server.consensus.pendingMux.RUnlock()

	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Approve execution request
func PostConsensusApprove(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for PostConsensusApprove")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	user := getUser(r)
	if !user.HasRole("approver") {
		jr.Error("User not allowed for PostConsensusApprove")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Vote
	id := strings.TrimSpace(r.PostFormValue("id"))
	req := server.consensus.Get(id)
	if req == nil {
		jr.Error("Request not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	res := req.Approve(user)
	server.consensus.save()

	jr.Set("approved", res)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Cancel execution request
func DeleteConsensusRequest(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for  DeleteConsensusRequest")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	user := getUser(r)
	if !user.HasRole("requester") {
		jr.Error("User not allowed to DeleteConsensusRequest")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get template
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	req := server.consensus.Get(id)
	if req == nil {
		jr.Error("Request not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Did we request this? Or are we admin
	isAdmin := user.HasRole("admin")
	isCreator := req.RequestUserId == user.Id
	if !isAdmin && !isCreator {
		jr.Error("Only the creator or admins can cancel a request")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	//remove pending executions
	err := server.agentService.AbortConsensusExecution(req)
	//remove itself
	server.consensus.Abort(req, user)

	jr.Set("cancelled", err == nil)

	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Create execution request
func PostConsensusRequest(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for PostConsensusRequest")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Are we allow to request execution?
	user := getUser(r)
	if !user.HasRole("requester") {
		jr.Error("User not allowed to PostConsensusRequest")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Verify two factor for, so that a hacked account can not request or execute anything without getting access to the 2fa device
	if res, _ := user.ValidateTotp(r.PostFormValue("totp")); res == false {
		jr.Error("Invalid two factor token")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Reason
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if len(reason) < 4 {
		jr.Error("Please provide a valid reason")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Template
	templateId := strings.TrimSpace(r.PostFormValue("template"))
	clientIds := strings.Split(strings.TrimSpace(r.PostFormValue("clients")), ",")

	// Create request
	cr := server.consensus.AddRequest(templateId, clientIds, user, reason)
	cr.check() // Check whether it can run straight away
	server.consensus.save()

	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Create validation rule for templates
func PostTemplateValidation(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for PostTemplateValidation")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get template
	id := ps.ByName("templateid")
	template := server.templateStore.Get(id)
	if template == nil {
		jr.Error("Template not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Input
	txt := r.PostFormValue("text")
	isFatal := r.PostFormValue("fatal") == "1"
	mustContain := r.PostFormValue("must_contain") == "1"
	streamId := 1 // Default process output stream only

	// Text must have length
	if len(strings.TrimSpace(txt)) < 1 {
		jr.Error("Text can not be empty")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Create rule
	rule := newExecutionValidation(txt, isFatal, mustContain, streamId)

	// Add rule
	template.AddValidationRule(rule)

	// Save
	res := server.templateStore.save()

	// Done
	jr.Set("saved", res)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Delete validation rule from template
func DeleteTemplateValidation(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for DeleteTemplateValidation")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get template
	templateId := ps.ByName("templateid")
	template := server.templateStore.Get(templateId)
	if template == nil {
		jr.Error("Template not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Validaton rule id
	id := ps.ByName("id")

	// Delete rule
	template.DeleteValidationRule(id)

	// Save
	res := server.templateStore.save()

	// Done
	jr.Set("saved", res)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Get templates
func GetTemplate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetTemplate")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	server.templateStore.templateMux.RLock()
	jr.Set("templates", server.templateStore.Templates)
	server.templateStore.templateMux.RUnlock()
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Create template
func PostTemplate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for PostTemplate")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	user := getUser(r)
	if !user.HasRole("admin") {
		jr.Error("User not allowed to PostTemplate")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	title := strings.TrimSpace(r.PostFormValue("title"))
	description := strings.TrimSpace(r.PostFormValue("description"))
	command := r.PostFormValue("command")
	includedTags := r.PostFormValue("includedTags")
	excludedTags := r.PostFormValue("excludedTags")
	executionStrategyStr := r.PostFormValue("executionStrategy")

	// Create strategy
	var executionStrategy *ExecutionStrategy
	switch executionStrategyStr {
	case "simple":
		executionStrategy = newExecutionStrategy(SimpleExecutionStrategy)
		break
	case "one-test":
		executionStrategy = newExecutionStrategy(OneTestExecutionStrategy)
		break
	case "rolling":
		executionStrategy = newExecutionStrategy(RollingExecutionStrategy)
		break
	case "exponential-rolling":
		executionStrategy = newExecutionStrategy(ExponentialRollingExecutionStrategy)
		break
	default:
		jr.Error("Strategy not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Minimum authorizations
	minAuthStr := strings.TrimSpace(r.PostFormValue("minAuth"))
	minAuth, minAuthE := strconv.ParseInt(minAuthStr, 10, 0)
	if len(minAuthStr) < 1 {
		jr.Error("Fill in min auth")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	} else if minAuthE != nil {
		jr.Error(fmt.Sprintf("%s", minAuthE))
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	} else if minAuth < 1 {
		jr.Error("Min auth must be at least 1")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Timeout
	timeoutStr := strings.TrimSpace(r.PostFormValue("timeout"))
	timeout, timeoutE := strconv.ParseInt(timeoutStr, 10, 0)
	if len(timeoutStr) < 1 {
		jr.Error("Fill in timeout")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	} else if timeoutE != nil {
		jr.Error(fmt.Sprintf("%s", timeoutE))
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	} else if timeout < 1 {
		jr.Error("Timeout must be at least 1 second")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Validate template
	template := newTemplate(title, description, command, true, strings.Split(includedTags, ","), strings.Split(excludedTags, ","), uint(minAuth), int(timeout), executionStrategy)
	valid, err := template.IsValid()
	if !valid {
		jr.Error(fmt.Sprintf("%s", err))
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	server.templateStore.Add(template)
	server.templateStore.save()
	jr.Set("template", template)
	jr.Set("saved", true)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Login
func PostAuth(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()

	authReq := &AuthRequest{
		login:      strings.TrimSpace(r.PostFormValue("username")),
		credential: strings.TrimSpace(r.PostFormValue("password")),
		token:      strings.TrimSpace(r.PostFormValue("2fa")),
	}

	user, err := server.authService.authUser(authReq)
	if err != nil {
		log.Printf("%s\n", err)
		jr.Error("Username / password / two-factor combination invalid") // Message must be constant to not leak information
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Start setssion
	token := user.StartSession()
	user.TouchSession(getIp(r))
	server.userStore.save() // Call save to persist token

	// Return token
	jr.Set("session_token", token)

	// User roles
	roles := make([]string, 0)
	for role := range user.Roles {
		roles = append(roles, role)
	}
	jr.Set("user_roles", roles)
	jr.Set("user_id", user.Id)
	jr.Set("two_factor_enabled", user.HasTwoFactor())
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// List of all tags
func GetTags(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetTags")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	server.tagsMux.RLock()
	tags := make([]string, 0)
	for tag := range server.Tags {
		tags = append(tags, tag)
	}
	jr.Set("tags", tags)
	server.tagsMux.RUnlock()
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Change password
func PutUserPassword(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for PutUserPassword")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Validate password
	newPwd := r.PostFormValue("password")
	if len(newPwd) < 16 {
		jr.Error("Password must be at least 16 characters, please pick a strong one!")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Match passwords
	newPwd2 := r.PostFormValue("password2")
	if newPwd != newPwd2 {
		jr.Error("Please confirm your password")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get user
	user := getUser(r)
	if user == nil {
		return
	}

	// Change password
	user.PasswordHash, _ = server.userStore.HashPassword(newPwd)
	server.userStore.save()

	jr.Set("saved", true)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// User from request
func getUser(r *http.Request) *User {
	// Username
	usr := r.Header.Get("X-Auth-User")

	// Get user
	user := server.userStore.ByName(usr)
	if user == nil {
		return nil
	}

	// Has token?
	if len(user.SessionToken) < 1 {
		return nil
	}

	// Enabled?
	if user.Enabled == false {
		return nil
	}

	// Token expired
	if time.Now().Sub(user.SessionLastTimestamp) > time.Duration(30*time.Minute) {
		return nil
	}

	// Validate token match
	if r.Header.Get("X-Auth-Session") != user.SessionToken {
		return nil
	}
	return user
}

// Delete template
func DeleteTemplate(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for DeleteTemplate")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	usr := getUser(r)
	if !usr.HasRole("admin") {
		jr.Error("User not allowed to DeleteTemplate")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Username
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	// Make sure it's not used by an HTTP check
	if len(server.httpCheckStore.FindByTemplate(id)) > 0 {
		jr.Error("This template is used by one or multiple http checks. You need to remove those first before deleting the template.")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Remove
	server.templateStore.Remove(id)
	server.templateStore.save()

	jr.Set("saved", true)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Delete user
func DeleteUser(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for DeleteUser")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	usr := getUser(r)
	if !usr.HasRole("admin") {
		jr.Error("User not allowed to DeleteUser")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Verify two factor for deletion of a user
	if res, _ := usr.ValidateTotp(r.URL.Query().Get("admin_totp")); res == false {
		jr.Error("Invalid two factor token")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Username
	username := strings.TrimSpace(r.URL.Query().Get("username"))

	// Can not remove yourself
	if usr.Username == username {
		jr.Error("You can not remove yourself. If you want to achieve this, make a new admin account. Login as that new account and then remove the old account.")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get user
	server.userStore.RemoveByName(username)
	server.userStore.save()

	jr.Set("saved", true)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Create user
func PostUser(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for PostUser")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	usr := getUser(r)
	if !usr.HasRole("admin") {
		jr.Error("User not allowed to PostUser")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Verify two factor for creation of new user, so that a hacked admin can not create a new user and use that to sign of for new commands
	if res, _ := usr.ValidateTotp(r.PostFormValue("admin_totp")); res == false {
		jr.Error("Invalid two factor token")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Username
	username := r.PostFormValue("username")
	email := r.PostFormValue("email")

	// Validate password
	newPwd := r.PostFormValue("password")
	if len(newPwd) < 16 {
		jr.Error("Password must be at least 16 characters, please pick a strong one!")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Match passwords
	newPwd2 := r.PostFormValue("password2")
	if newPwd != newPwd2 {
		jr.Error("Please confirm your password")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Roles
	roles := strings.Split(r.PostFormValue("roles"), ",")

	// Create user
	res := server.userStore.CreateUser(username, newPwd, email, roles)
	server.userStore.save()

	jr.Set("saved", res)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Modify user
func PutUser(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for Change User")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	admin := getUser(r)
	if !admin.HasRole("admin") {
		jr.Error("User not allowed to Change User")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Verify two factor for change user
	if res, _ := admin.ValidateTotp(r.PostFormValue("token")); res == false {
		jr.Error("Invalid two factor token")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Username
	username := r.PostFormValue("username")
	user := server.userStore.ByName(username)
	if user == nil {
		jr.Error("Cannot find user to modify")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	for key, _ := range r.PostForm {
		switch key {
		case "enable":
			user.Enabled = cast.ToBool(r.PostFormValue(key))
		case "username", "token":
			continue
		default:
			jr.Error("Invalid change request")
			fmt.Fprint(w, jr.ToString(conf.Debug))
			return
		}
	}

	server.userStore.save()
	jr.Set("changed", true)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Get user names
func GetUsersNames(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetUsersNames")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	// Availble to anyone
	server.userStore.usersMux.RLock()
	users := make([]map[string]interface{}, 0)
	for _, userPtr := range server.userStore.Users {
		user := make(map[string]interface{})
		user["Id"] = userPtr.Id
		user["Username"] = userPtr.Username
		users = append(users, user)
	}
	jr.Set("users", users)
	server.userStore.usersMux.RUnlock()
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// List users
func GetUsers(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetUsers")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	usr := getUser(r)
	if !usr.HasRole("admin") {
		jr.Error("User not allowed to GetUsers")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	server.userStore.usersMux.RLock()
	users := make([]User, 0)
	for _, userPtr := range server.userStore.Users {
		user := *userPtr
		// Hide sensitive fields
		user.PasswordHash = ""
		user.SessionToken = ""
		user.TotpSecret = ""
		users = append(users, user)
	}
	jr.Set("users", users)
	jr.Set("authTypes", server.userStore.AuthTypes())
	server.userStore.usersMux.RUnlock()
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// List clients
func GetClients(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !authUser(r) {
		jr.Error("User not authorized for GetClients")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Filters
	tagsInclude := strings.Split(r.URL.Query().Get("filter_tags_include"), ",")
	tagsExclude := strings.Split(r.URL.Query().Get("filter_tags_exclude"), ",")
	if len(tagsInclude) == 1 && tagsInclude[0] == "" {
		tagsInclude = make([]string, 0)
	}
	if len(tagsExclude) == 1 && tagsExclude[0] == "" {
		tagsExclude = make([]string, 0)
	}

	clients := make([]RegisteredClient, 0)
	clientList, _ := server.agentService.List(tagsInclude, tagsExclude)

	for _, clientPtr := range clientList {
		// Deref, so we can modify the object without modifying the real one
		//TODO unsafe
		client := *clientPtr.(*RegisteredClient)

		// Clear out the dispatched commands history (massive logs etc)
		client.DispatchedCmds = nil
		client.Cmds = nil

		// Add to list
		clients = append(clients, client)
	}

	jr.Set("clients", clients)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Register client with token, this is used for signing commands towards the client which will then verify them
func PostClientAuth(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !auth(r) {
		jr.Error("User not authorized for PostClientAuth")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get client
	registeredClient := server.GetClient(ps.ByName("clientId"))
	if registeredClient == nil {
		jr.Error("Client not registered")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Generate token and return
	token, tokenE := secureRandomString(32)
	if tokenE != nil {
		jr.Error("Failed to generate token")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Store token
	log.Printf(fmt.Sprintf("Client %s authenticated", registeredClient.ClientId))
	registeredClient.mux.Lock()
	registeredClient.AuthToken = token
	registeredClient.mux.Unlock()

	// Sign token based of our secure token
	hasher := sha256.New()
	hasher.Write([]byte(token))
	hasher.Write([]byte(conf.Token))
	tokenSignature := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

	// Return token
	jr.Set("token", token)
	jr.Set("token_signature", tokenSignature)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Set command logs
func PutClientCmdLogs(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !auth(r) {
		jr.Error("Client not authorized for PutClientCmdLogs")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get client
	registeredClient := server.GetClient(ps.ByName("clientId"))
	if registeredClient == nil {
		jr.Error("Client not registered")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Command
	cmdId := ps.ByName("cmd")
	registeredClient.mux.RLock()
	cmd := registeredClient.DispatchedCmds[cmdId]
	registeredClient.mux.RUnlock()
	if cmd == nil {
		jr.Error("Command not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Read body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		jr.Error("Failed to read body")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Decode json
	type LogStruct struct {
		Output []string `json:"output"`
		Error  []string `json:"error"`
	}
	var m *LogStruct
	je := json.Unmarshal(body, &m)
	if je != nil {
		jr.Error("Failed to parse json")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Append buffers
	if m.Output != nil {
		for _, line := range m.Output {
			cmd.BufOutput = append(cmd.BufOutput, line)
		}
	}

	// Append buffers
	if m.Error != nil {
		for _, line := range m.Error {
			cmd.BufOutputErr = append(cmd.BufOutputErr, line)
		}
	}

	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Set command state
func PutClientCmdState(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !auth(r) {
		jr.Error("Client not authorized for PutClientCmdState")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get client
	registeredClient := server.GetClient(ps.ByName("clientId"))
	if registeredClient == nil {
		jr.Error("Client not registered")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Command
	cmdId := ps.ByName("cmd")
	registeredClient.mux.RLock()
	cmd := registeredClient.DispatchedCmds[cmdId]
	registeredClient.mux.RUnlock()
	if cmd == nil {
		jr.Error("Command not found")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// State
	state := r.URL.Query().Get("state")

	// Save state in local server
	cmd.SetState(state)

	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Commands
func ClientCmds(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !auth(r) {
		jr.Error("Client not authorized for ClientCmds")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Get client
	clientId := ps.ByName("clientId")
	registeredClient := server.GetClient(clientId)
	if registeredClient == nil {
		jr.Error(fmt.Sprintf("Client %s not registered", clientId))
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Do we have a token? If not, ignore as the client will discard the commands without hmac signatures
	if len(registeredClient.AuthToken) < 1 {
		jr.Error(fmt.Sprintf("Client %s auth token not available", registeredClient.ClientId))
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}

	// Read from channel and dispatch before timeout
	select {
	case <-registeredClient.CmdChan:
		cmds := make([]*Cmd, 0)
		registeredClient.mux.Lock()
		for _, cmd := range registeredClient.Cmds {
			if cmd.Pending {
				cmds = append(cmds, cmd)
				cmd.Pending = false
			}
		}
		registeredClient.mux.Unlock()
		jr.Set("cmds", cmds)
	case <-time.After(time.Second * LONG_POLL_TIMEOUT):
		// No commands
		jr.Set("cmds", make([]string, 0))
	}
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Ping
func ClientPing(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	jr := jresp.NewJsonResp()
	if !auth(r) {
		jr.Error("Client not authorized for ClientPing")
		fmt.Fprint(w, jr.ToString(conf.Debug))
		return
	}
	tags := strings.Split(r.URL.Query().Get("tags"), ",")
	server.RegisterClient(ps.ByName("clientId"), tags)
	jr.Set("ack", true)
	jr.Set("server_instance_id", server.InstanceId)
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Home
func Home(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	// Redirect to console
	http.Redirect(w, r, r.URL.String()+"console/", 301)
}

// Ping
func Ping(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	jr := jresp.NewJsonResp()
	jr.Set("ping", "pong")
	jr.OK()
	fmt.Fprint(w, jr.ToString(conf.Debug))
}

// Auth
func auth(r *http.Request) bool {
	// Signed token
	uri := r.URL.String()
	hasher := sha256.New()
	hasher.Write([]byte(uri))
	hasher.Write([]byte(conf.Token))
	signedToken := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

	// Validate
	if r.Header.Get("X-Auth") != signedToken {
		return false
	}
	return true
}

// Auth user
func authUser(r *http.Request) bool {
	// Username
	user := getUser(r)
	if user == nil {
		return false
	}

	user.TouchSession(getIp(r))
	return true
}

// Get ip
func getIp(r *http.Request) string {
	return r.RemoteAddr
}

// Create new server
func newServer(as AgentService) *Server {
	id, _ := uuid.NewV4()
	return &Server{
		Tags:         make(map[string]bool),
		InstanceId:   id.String(),
		agentService: as,
	}
}

// New registered client
func newRegisteredClient(clientId string) *RegisteredClient {
	return &RegisteredClient{
		ClientId:       clientId,
		Cmds:           make(map[string]*Cmd),
		CmdChan:        make(chan bool),
		DispatchedCmds: make(map[string]*Cmd),
	}
}
