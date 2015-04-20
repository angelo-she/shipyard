package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	r "github.com/dancannon/gorethink"
	"github.com/gorilla/sessions"
	"github.com/samalba/dockerclient"
	"github.com/shipyard/shipyard"
	"github.com/shipyard/shipyard/auth"
	"github.com/shipyard/shipyard/dockerhub"
	"github.com/shipyard/shipyard/version"
)

const (
	tblNameConfig      = "config"
	tblNameEvents      = "events"
	tblNameAccounts    = "accounts"
	tblNameRoles       = "roles"
	tblNameServiceKeys = "service_keys"
	tblNameExtensions  = "extensions"
	tblNameWebhookKeys = "webhook_keys"
	tblNameRegistries  = "registries"
	tblNameConsole     = "console"
	storeKey           = "shipyard"
	trackerHost        = "http://tracker.shipyard-project.com"
	NodeHealthUp       = "up"
	NodeHealthDown     = "down"
)

var (
	ErrAccountExists              = errors.New("account already exists")
	ErrAccountDoesNotExist        = errors.New("account does not exist")
	ErrRoleDoesNotExist           = errors.New("role does not exist")
	ErrNodeDoesNotExist           = errors.New("node does not exist")
	ErrServiceKeyDoesNotExist     = errors.New("service key does not exist")
	ErrInvalidAuthToken           = errors.New("invalid auth token")
	ErrExtensionDoesNotExist      = errors.New("extension does not exist")
	ErrWebhookKeyDoesNotExist     = errors.New("webhook key does not exist")
	ErrRegistryDoesNotExist       = errors.New("registry does not exist")
	ErrConsoleSessionDoesNotExist = errors.New("console session does not exist")
	store                         = sessions.NewCookieStore([]byte(storeKey))
)

type (
	DefaultManager struct {
		storeKey         string
		database         string
		authKey          string
		session          *r.Session
		authenticator    *auth.Authenticator
		store            *sessions.CookieStore
		client           *dockerclient.DockerClient
		disableUsageInfo bool
	}

	Manager interface {
		Accounts() ([]*auth.Account, error)
		Account(username string) (*auth.Account, error)
		Authenticate(username, password string) bool
		SaveAccount(account *auth.Account) error
		DeleteAccount(account *auth.Account) error
		Roles() ([]*auth.Role, error)
		Role(name string) (*auth.Role, error)
		SaveRole(role *auth.Role) error
		DeleteRole(role *auth.Role) error
		Store() *sessions.CookieStore
		StoreKey() string
		Container(id string) (*dockerclient.ContainerInfo, error)
		SaveServiceKey(key *auth.ServiceKey) error
		RemoveServiceKey(key string) error
		SaveEvent(event *shipyard.Event) error
		Events(limit int) ([]*shipyard.Event, error)
		PurgeEvents() error
		ServiceKey(key string) (*auth.ServiceKey, error)
		ServiceKeys() ([]*auth.ServiceKey, error)
		NewAuthToken(username string, userAgent string) (*auth.AuthToken, error)
		VerifyAuthToken(username, token string) error
		VerifyServiceKey(key string) error
		NewServiceKey(description string) (*auth.ServiceKey, error)
		ChangePassword(username, password string) error
		WebhookKey(key string) (*dockerhub.WebhookKey, error)
		WebhookKeys() ([]*dockerhub.WebhookKey, error)
		NewWebhookKey(image string) (*dockerhub.WebhookKey, error)
		SaveWebhookKey(key *dockerhub.WebhookKey) error
		DeleteWebhookKey(id string) error
		DockerClient() *dockerclient.DockerClient

		Nodes() ([]*shipyard.Node, error)
		Node(name string) (*shipyard.Node, error)

		AddRegistry(registry *shipyard.Registry) error
		RemoveRegistry(registry *shipyard.Registry) error
		Registries() ([]*shipyard.Registry, error)
		Registry(name string) (*shipyard.Registry, error)

		CreateConsoleSession(c *shipyard.ConsoleSession) error
		RemoveConsoleSession(c *shipyard.ConsoleSession) error
		ConsoleSession(token string) (*shipyard.ConsoleSession, error)
		ValidateConsoleSessionToken(containerId, token string) bool
	}
)

func NewManager(addr string, database string, authKey string, client *dockerclient.DockerClient, disableUsageInfo bool) (Manager, error) {
	session, err := r.Connect(r.ConnectOpts{
		Address:     addr,
		Database:    database,
		AuthKey:     authKey,
		MaxIdle:     10,
		IdleTimeout: time.Second * 30,
	})
	if err != nil {
		return nil, err
	}
	log.Info("checking database")

	r.DbCreate(database).Run(session)
	m := &DefaultManager{
		database:         database,
		authKey:          authKey,
		session:          session,
		authenticator:    &auth.Authenticator{},
		store:            store,
		client:           client,
		storeKey:         storeKey,
		disableUsageInfo: disableUsageInfo,
	}
	m.initdb()
	m.init()
	return m, nil
}

func (m DefaultManager) Store() *sessions.CookieStore {
	return m.store
}

func (m DefaultManager) DockerClient() *dockerclient.DockerClient {
	return m.client
}

func (m DefaultManager) StoreKey() string {
	return m.storeKey
}

func (m DefaultManager) initdb() {
	// create tables if needed
	tables := []string{tblNameConfig, tblNameEvents, tblNameAccounts, tblNameRoles, tblNameConsole, tblNameServiceKeys, tblNameRegistries, tblNameExtensions, tblNameWebhookKeys}
	for _, tbl := range tables {
		_, err := r.Table(tbl).Run(m.session)
		if err != nil {
			if _, err := r.Db(m.database).TableCreate(tbl).Run(m.session); err != nil {
				log.Fatalf("error creating table: %s", err)
			}
		}
	}
}

func (m DefaultManager) init() error {
	// anonymous usage info
	go m.usageReport()
	return nil
}

func (m DefaultManager) usageReport() {
	if m.disableUsageInfo {
		return
	}
	m.uploadUsage()
	t := time.NewTicker(1 * time.Hour).C
	for {
		select {
		case <-t:
			go m.uploadUsage()
		}
	}
}

func (m DefaultManager) uploadUsage() {
	id := "anon"
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Name != "lo" {
				hw := iface.HardwareAddr.String()
				id = strings.Replace(hw, ":", "", -1)
				break
			}
		}
	}
	usage := &shipyard.Usage{
		ID:      id,
		Version: version.Version,
	}
	b, err := json.Marshal(usage)
	if err != nil {
		log.Warnf("error serializing usage info: %s", err)
	}
	buf := bytes.NewBuffer(b)
	if _, err := http.Post(fmt.Sprintf("%s/update", trackerHost), "application/json", buf); err != nil {
		log.Warnf("error sending usage info: %s", err)
	}
}

func (m DefaultManager) Container(id string) (*dockerclient.ContainerInfo, error) {
	return m.client.InspectContainer(id)
}

func (m DefaultManager) SaveServiceKey(key *auth.ServiceKey) error {
	if _, err := r.Table(tblNameServiceKeys).Insert(key).RunWrite(m.session); err != nil {
		return err
	}
	m.init()
	evt := &shipyard.Event{
		Type:    "add-service-key",
		Time:    time.Now(),
		Message: fmt.Sprintf("description=%s", key.Description),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) RemoveServiceKey(key string) error {
	k, err := m.ServiceKey(key)
	if err != nil {
		return err
	}
	evt := &shipyard.Event{
		Type:    "remove-service-key",
		Time:    time.Now(),
		Message: fmt.Sprintf("description=%s", k.Description),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	if _, err := r.Table(tblNameServiceKeys).Filter(map[string]string{"key": key}).Delete().RunWrite(m.session); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) SaveEvent(event *shipyard.Event) error {
	if _, err := r.Table(tblNameEvents).Insert(event).RunWrite(m.session); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) Events(limit int) ([]*shipyard.Event, error) {
	t := r.Table(tblNameEvents).OrderBy(r.Desc("Time"))
	if limit > -1 {
		t.Limit(limit)
	}
	res, err := t.Run(m.session)
	if err != nil {
		return nil, err
	}
	events := []*shipyard.Event{}
	if err := res.All(&events); err != nil {
		return nil, err
	}
	return events, nil
}

func (m DefaultManager) PurgeEvents() error {
	if _, err := r.Table(tblNameEvents).Delete().RunWrite(m.session); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) ServiceKey(key string) (*auth.ServiceKey, error) {
	res, err := r.Table(tblNameServiceKeys).Filter(map[string]string{"key": key}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrServiceKeyDoesNotExist
	}
	var k *auth.ServiceKey
	if err := res.One(&k); err != nil {
		return nil, err
	}
	return k, nil
}

func (m DefaultManager) ServiceKeys() ([]*auth.ServiceKey, error) {
	res, err := r.Table(tblNameServiceKeys).Run(m.session)
	if err != nil {
		return nil, err
	}
	keys := []*auth.ServiceKey{}
	if err := res.All(&keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func (m DefaultManager) Accounts() ([]*auth.Account, error) {
	res, err := r.Table(tblNameAccounts).OrderBy(r.Asc("username")).Run(m.session)
	if err != nil {
		return nil, err
	}
	accounts := []*auth.Account{}
	if err := res.All(&accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}

func (m DefaultManager) Account(username string) (*auth.Account, error) {
	res, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrAccountDoesNotExist
	}
	var account *auth.Account
	if err := res.One(&account); err != nil {
		return nil, err
	}
	return account, nil
}

func (m DefaultManager) SaveAccount(account *auth.Account) error {
	var (
		hash      string
		eventType string
	)
	if account.Password != "" {
		h, err := m.authenticator.Hash(account.Password)
		if err != nil {
			return err
		}

		hash = h
	}
	// check if exists; if so, update
	acct, err := m.Account(account.Username)
	if err != nil && err != ErrAccountDoesNotExist {
		return err
	}

	// update
	if acct != nil {
		updates := map[string]string{
			"first_name": account.FirstName,
			"last_name":  account.LastName,
		}
		if account.Password != "" {
			updates["password"] = hash
		}

		if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": account.Username}).Update(updates).RunWrite(m.session); err != nil {
			return err
		}

		eventType = "update-account"
	} else {
		account.Password = hash
		if _, err := r.Table(tblNameAccounts).Insert(account).RunWrite(m.session); err != nil {
			return err
		}
		eventType = "add-account"
	}

	evt := &shipyard.Event{
		Type:    eventType,
		Time:    time.Now(),
		Message: fmt.Sprintf("username=%s", account.Username),
		Tags:    []string{"security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) DeleteAccount(account *auth.Account) error {
	res, err := r.Table(tblNameAccounts).Filter(map[string]string{"id": account.ID}).Delete().Run(m.session)
	if err != nil {
		return err
	}
	if res.IsNil() {
		return ErrAccountDoesNotExist
	}
	evt := &shipyard.Event{
		Type:    "delete-account",
		Time:    time.Now(),
		Message: fmt.Sprintf("username=%s", account.Username),
		Tags:    []string{"security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) Roles() ([]*auth.Role, error) {
	res, err := r.Table(tblNameRoles).OrderBy(r.Asc("name")).Run(m.session)
	if err != nil {
		return nil, err
	}
	roles := []*auth.Role{}
	if err := res.All(&roles); err != nil {
		return nil, err
	}
	return roles, nil
}

func (m DefaultManager) Role(name string) (*auth.Role, error) {
	res, err := r.Table(tblNameRoles).Filter(map[string]string{"name": name}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrRoleDoesNotExist
	}
	var role *auth.Role
	if err := res.One(&role); err != nil {
		return nil, err
	}
	return role, nil
}

func (m DefaultManager) SaveRole(role *auth.Role) error {
	if _, err := r.Table(tblNameRoles).Insert(role).RunWrite(m.session); err != nil {
		return err
	}

	evt := &shipyard.Event{
		Type:    "add-role",
		Time:    time.Now(),
		Message: fmt.Sprintf("name=%s", role.Name),
		Tags:    []string{"cluster", "security"},
	}

	if err := m.SaveEvent(evt); err != nil {
		return err
	}

	return nil
}

func (m DefaultManager) DeleteRole(role *auth.Role) error {
	res, err := r.Table(tblNameRoles).Get(role.ID).Delete().Run(m.session)
	if err != nil {
		return err
	}
	if res.IsNil() {
		return ErrRoleDoesNotExist
	}
	evt := &shipyard.Event{
		Type:    "delete-role",
		Time:    time.Now(),
		Message: fmt.Sprintf("name=%s", role.Name),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) Authenticate(username, password string) bool {
	acct, err := m.Account(username)
	if err != nil {
		log.Error(err)
		return false
	}
	evt := &shipyard.Event{
		Type:    "login",
		Time:    time.Now(),
		Message: fmt.Sprintf("username=%s", acct.Username),
		Tags:    []string{"login", "security"},
	}
	// do not return a fail if error happens upon saving even; still want login
	_ = m.SaveEvent(evt)

	return m.authenticator.Authenticate(password, acct.Password)
}

func (m DefaultManager) NewAuthToken(username string, userAgent string) (*auth.AuthToken, error) {
	tk, err := m.authenticator.GenerateToken()
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	acct, err := m.Account(username)
	if err != nil {
		return nil, err
	}
	token := &auth.AuthToken{}
	tokens := acct.Tokens
	found := false
	for _, t := range tokens {
		if t.UserAgent == userAgent {
			found = true
			t.Token = tk
			token = t
			break
		}
	}
	if !found {
		token = &auth.AuthToken{
			UserAgent: userAgent,
			Token:     tk,
		}
		tokens = append(tokens, token)
	}
	// delete token
	if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Filter(r.Row.Field("user_agent").Eq(userAgent)).Delete().Run(m.session); err != nil {
		return nil, err
	}
	// add
	if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Update(map[string]interface{}{"tokens": tokens}).RunWrite(m.session); err != nil {
		return nil, err
	}
	return token, nil
}

func (m DefaultManager) VerifyAuthToken(username, token string) error {
	acct, err := m.Account(username)
	if err != nil {
		return err
	}
	found := false
	for _, t := range acct.Tokens {
		if token == t.Token {
			found = true
			break
		}
	}
	if !found {
		return ErrInvalidAuthToken
	}
	return nil
}

func (m DefaultManager) VerifyServiceKey(key string) error {
	if _, err := m.ServiceKey(key); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) NewServiceKey(description string) (*auth.ServiceKey, error) {
	k, err := m.authenticator.GenerateToken()
	if err != nil {
		return nil, err
	}
	key := &auth.ServiceKey{
		Key:         k[24:],
		Description: description,
	}
	if err := m.SaveServiceKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

func (m DefaultManager) ChangePassword(username, password string) error {
	hash, err := m.authenticator.Hash(password)
	if err != nil {
		return err
	}
	if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Update(map[string]string{"password": hash}).Run(m.session); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) WebhookKey(key string) (*dockerhub.WebhookKey, error) {
	res, err := r.Table(tblNameWebhookKeys).Filter(map[string]string{"key": key}).Run(m.session)
	if err != nil {
		return nil, err

	}

	if res.IsNil() {
		return nil, ErrWebhookKeyDoesNotExist

	}

	var k *dockerhub.WebhookKey
	if err := res.One(&k); err != nil {
		return nil, err

	}

	return k, nil
}

func (m DefaultManager) WebhookKeys() ([]*dockerhub.WebhookKey, error) {
	res, err := r.Table(tblNameWebhookKeys).OrderBy(r.Asc("image")).Run(m.session)
	if err != nil {
		return nil, err
	}
	keys := []*dockerhub.WebhookKey{}
	if err := res.All(&keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func (m DefaultManager) NewWebhookKey(image string) (*dockerhub.WebhookKey, error) {
	k := generateId(16)
	key := &dockerhub.WebhookKey{
		Key:   k,
		Image: image,
	}

	if err := m.SaveWebhookKey(key); err != nil {
		return nil, err
	}

	return key, nil
}

func (m DefaultManager) SaveWebhookKey(key *dockerhub.WebhookKey) error {
	if _, err := r.Table(tblNameWebhookKeys).Insert(key).RunWrite(m.session); err != nil {
		return err

	}

	evt := &shipyard.Event{
		Type:    "add-webhook-key",
		Time:    time.Now(),
		Message: fmt.Sprintf("image=%s", key.Image),
		Tags:    []string{"docker", "webhook"},
	}

	if err := m.SaveEvent(evt); err != nil {
		return err
	}

	return nil
}

func (m DefaultManager) DeleteWebhookKey(id string) error {
	key, err := m.WebhookKey(id)
	if err != nil {
		return err

	}
	res, err := r.Table(tblNameWebhookKeys).Get(key.ID).Delete().Run(m.session)
	if err != nil {
		return err

	}

	if res.IsNil() {
		return ErrWebhookKeyDoesNotExist

	}

	evt := &shipyard.Event{
		Type:    "delete-webhook-key",
		Time:    time.Now(),
		Message: fmt.Sprintf("image=%s key=%s", key.Image, key.Key),
		Tags:    []string{"docker", "webhook"},
	}

	if err := m.SaveEvent(evt); err != nil {
		return err

	}

	return nil
}

func (m DefaultManager) Nodes() ([]*shipyard.Node, error) {
	info, err := m.client.Info()
	if err != nil {
		return nil, err
	}

	nodes, err := parseClusterNodes(info.DriverStatus)
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

func (m DefaultManager) Node(name string) (*shipyard.Node, error) {
	nodes, err := m.Nodes()
	if err != nil {
		return nil, err
	}

	for _, node := range nodes {
		if node.Name == name {
			return node, nil
		}
	}

	return nil, nil
}

func (m DefaultManager) AddRegistry(registry *shipyard.Registry) error {
	if _, err := r.Table(tblNameRegistries).Insert(registry).RunWrite(m.session); err != nil {
		return err
	}

	evt := &shipyard.Event{
		Type:    "add-registry",
		Time:    time.Now(),
		Message: fmt.Sprintf("name=%s addr=%s", registry.Name, registry.Addr),
		Tags:    []string{"registry", "security"},
	}

	if err := m.SaveEvent(evt); err != nil {
		return err
	}

	return nil
}

func (m DefaultManager) RemoveRegistry(registry *shipyard.Registry) error {
	res, err := r.Table(tblNameRegistries).Get(registry.ID).Delete().Run(m.session)
	if err != nil {
		return err
	}
	if res.IsNil() {
		return ErrRoleDoesNotExist
	}
	evt := &shipyard.Event{
		Type:    "delete-registry",
		Time:    time.Now(),
		Message: fmt.Sprintf("name=%s addr=%s", registry.Name, registry.Addr),
		Tags:    []string{"registry", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) Registries() ([]*shipyard.Registry, error) {
	res, err := r.Table(tblNameRegistries).OrderBy(r.Asc("name")).Run(m.session)
	if err != nil {
		return nil, err
	}

	regs := []*shipyard.Registry{}
	if err := res.All(&regs); err != nil {
		return nil, err
	}

	registries := []*shipyard.Registry{}
	for _, r := range regs {
		reg, err := shipyard.NewRegistry(r.ID, r.Name, r.Addr)
		if err != nil {
			return nil, err
		}

		registries = append(registries, reg)
	}

	return registries, nil
}

func (m DefaultManager) Registry(name string) (*shipyard.Registry, error) {
	res, err := r.Table(tblNameRegistries).Filter(map[string]string{"name": name}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrRegistryDoesNotExist
	}
	var reg *shipyard.Registry
	if err := res.One(&reg); err != nil {
		return nil, err
	}

	registry, err := shipyard.NewRegistry(reg.ID, reg.Name, reg.Addr)
	if err != nil {
		return nil, err
	}

	return registry, nil
}

func (m DefaultManager) CreateConsoleSession(c *shipyard.ConsoleSession) error {
	if _, err := r.Table(tblNameConsole).Insert(c).RunWrite(m.session); err != nil {
		return err
	}

	evt := &shipyard.Event{
		Type:    "add-console-session",
		Time:    time.Now(),
		Message: fmt.Sprintf("container=%s token=%s", c.ContainerID, c.Token),
		Tags:    []string{"console", "cluster"},
	}

	if err := m.SaveEvent(evt); err != nil {
		return err
	}

	return nil
}

func (m DefaultManager) RemoveConsoleSession(c *shipyard.ConsoleSession) error {
	res, err := r.Table(tblNameConsole).Get(c.ID).Delete().Run(m.session)
	if err != nil {
		return err
	}

	if res.IsNil() {
		return ErrConsoleSessionDoesNotExist
	}

	evt := &shipyard.Event{
		Type:    "remove-console-session",
		Time:    time.Now(),
		Message: fmt.Sprintf("container=%s token=%s", c.ContainerID, c.Token),
		Tags:    []string{"console", "cluster"},
	}

	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m DefaultManager) ConsoleSession(token string) (*shipyard.ConsoleSession, error) {
	res, err := r.Table(tblNameConsole).Filter(map[string]string{"token": token}).Run(m.session)
	if err != nil {
		return nil, err
	}

	if res.IsNil() {
		return nil, ErrConsoleSessionDoesNotExist
	}

	var c *shipyard.ConsoleSession
	if err := res.One(&c); err != nil {
		return nil, err
	}

	return c, nil
}

func (m DefaultManager) ValidateConsoleSessionToken(containerId string, token string) bool {
	cs, err := m.ConsoleSession(token)
	if err != nil {
		log.Errorf("error validating console session token: %s", err)
		return false
	}

	if cs == nil || cs.ContainerID != containerId {
		log.Warnf("unauthorized token request: %s", token)
		return false
	}

	if err := m.RemoveConsoleSession(cs); err != nil {
		log.Error(err)
		return false
	}

	return true
}
