package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/vault/audit"
	"github.com/hashicorp/vault/helper/mlock"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/physical"
	"github.com/hashicorp/vault/shamir"
)

const (
	// coreSealConfigPath is the path used to store our seal configuration.
	// This value is stored in plaintext, since we must be able to read
	// it even with the Vault sealed. This is required so that we know
	// how many secret parts must be used to reconstruct the master key.
	coreSealConfigPath = "core/seal-config"

	// coreLockPath is the path used to acquire a coordinating lock
	// for a highly-available deploy.
	coreLockPath = "core/lock"

	// coreLeaderPrefix is the prefix used for the UUID that contains
	// the currently elected leader.
	coreLeaderPrefix = "core/leader/"

	// lockRetryInterval is the interval we re-attempt to acquire the
	// HA lock if an error is encountered
	lockRetryInterval = 10 * time.Second
)

var (
	// ErrSealed is returned if an operation is performed on
	// a sealed barrier. No operation is expected to succeed before unsealing
	ErrSealed = errors.New("Vault is sealed")

	// ErrStandby is returned if an operation is performed on
	// a standby Vault. No operation is expected to succeed until active.
	ErrStandby = errors.New("Vault is in standby mode")

	// ErrAlreadyInit is returned if the core is already
	// initialized. This prevents a re-initialization.
	ErrAlreadyInit = errors.New("Vault is already initialized")

	// ErrNotInit is returned if a non-initialized barrier
	// is attempted to be unsealed.
	ErrNotInit = errors.New("Vault is not initialized")

	// ErrInternalError is returned when we don't want to leak
	// any information about an internal error
	ErrInternalError = errors.New("internal error")

	// ErrHANotEnabled is returned if the operation only makes sense
	// in an HA setting
	ErrHANotEnabled = errors.New("Vault is not configured for highly-available mode")
)

// SealConfig is used to describe the seal configuration
type SealConfig struct {
	// SecretShares is the number of shares the secret is
	// split into. This is the N value of Shamir
	SecretShares int `json:"secret_shares"`

	// SecretThreshold is the number of parts required
	// to open the vault. This is the T value of Shamir
	SecretThreshold int `json:"secret_threshold"`
}

// Validate is used to sanity check the seal configuration
func (s *SealConfig) Validate() error {
	if s.SecretShares < 1 {
		return fmt.Errorf("secret shares must be at least one")
	}
	if s.SecretThreshold < 1 {
		return fmt.Errorf("secret threshold must be at least one")
	}
	if s.SecretShares > 255 {
		return fmt.Errorf("secret shares must be less than 256")
	}
	if s.SecretThreshold > 255 {
		return fmt.Errorf("secret threshold must be less than 256")
	}
	if s.SecretThreshold > s.SecretShares {
		return fmt.Errorf("secret threshold cannot be larger than secret shares")
	}
	return nil
}

// InitResult is used to provide the key parts back after
// they are generated as part of the initialization.
type InitResult struct {
	SecretShares [][]byte
	RootToken    string
}

// ErrInvalidKey is returned if there is an error with a
// provided unseal key.
type ErrInvalidKey struct {
	Reason string
}

func (e *ErrInvalidKey) Error() string {
	return fmt.Sprintf("invalid key: %v", e.Reason)
}

// Core is used as the central manager of Vault activity. It is the primary point of
// interface for API handlers and is responsible for managing the logical and physical
// backends, router, security barrier, and audit trails.
type Core struct {
	// HABackend may be available depending on the physical backend
	ha physical.HABackend

	// AdvertiseAddr is the address we advertise as leader if held
	advertiseAddr string

	// physical backend is the un-trusted backend with durable data
	physical physical.Backend

	// barrier is the security barrier wrapping the physical backend
	barrier SecurityBarrier

	// router is responsible for managing the mount points for logical backends.
	router *Router

	// logicalBackends is the mapping of backends to use for this core
	logicalBackends map[string]logical.Factory

	// credentialBackends is the mapping of backends to use for this core
	credentialBackends map[string]logical.Factory

	// auditBackends is the mapping of backends to use for this core
	auditBackends map[string]audit.Factory

	// stateLock protects mutable state
	stateLock sync.RWMutex
	sealed    bool

	standby       bool
	standbyDoneCh chan struct{}
	standbyStopCh chan struct{}

	// unlockParts has the keys provided to Unseal until
	// the threshold number of parts is available.
	unlockParts [][]byte

	// mounts is loaded after unseal since it is a protected
	// configuration
	mounts *MountTable

	// auth is loaded after unseal since it is a protected
	// configuration
	auth *MountTable

	// audit is loaded after unseal since it is a protected
	// configuration
	audit *MountTable

	// auditBroker is used to ingest the audit events and fan
	// out into the configured audit backends
	auditBroker *AuditBroker

	// systemView is the barrier view for the system backend
	systemView *BarrierView

	// expiration manager is used for managing LeaseIDs,
	// renewal, expiration and revocation
	expiration *ExpirationManager

	// rollback manager is used to run rollbacks periodically
	rollback *RollbackManager

	// policy store is used to manage named ACL policies
	policy *PolicyStore

	// token store is used to manage authentication tokens
	tokenStore *TokenStore

	// metricsCh is used to stop the metrics streaming
	metricsCh chan struct{}

	logger *log.Logger
}

// CoreConfig is used to parameterize a core
type CoreConfig struct {
	LogicalBackends    map[string]logical.Factory
	CredentialBackends map[string]logical.Factory
	AuditBackends      map[string]audit.Factory
	Physical           physical.Backend
	Logger             *log.Logger
	DisableCache       bool   // Disables the LRU cache on the physical backend
	DisableMlock       bool   // Disables mlock syscall
	CacheSize          int    // Custom cache size of zero for default
	AdvertiseAddr      string // Set as the leader address for HA
}

// NewCore isk used to construct a new core
func NewCore(conf *CoreConfig) (*Core, error) {
	// Check if this backend supports an HA configuraiton
	var haBackend physical.HABackend
	if ha, ok := conf.Physical.(physical.HABackend); ok {
		haBackend = ha
	}
	if haBackend != nil && conf.AdvertiseAddr == "" {
		return nil, fmt.Errorf("missing advertisement address")
	}

	// Wrap the backend in a cache unless disabled
	if !conf.DisableCache {
		_, isCache := conf.Physical.(*physical.Cache)
		_, isInmem := conf.Physical.(*physical.InmemBackend)
		if !isCache && !isInmem {
			cache := physical.NewCache(conf.Physical, conf.CacheSize)
			conf.Physical = cache
		}
	}

	if !conf.DisableMlock {
		// Ensure our memory usage is locked into physical RAM
		if err := mlock.LockMemory(); err != nil {
			return nil, fmt.Errorf(
				"Failed to lock memory: %v\n\n"+
					"This usually means that the mlock syscall is not available.\n"+
					"Vault uses mlock to prevent memory from being swapped to\n"+
					"disk. This requires root privileges as well as a machine\n"+
					"that supports mlock. Please enable mlock on your system or\n"+
					"disable Vault from using it. To disable Vault from using it,\n"+
					"set the `disable_mlock` configuration option in your configuration\n"+
					"file.",
				err)
		}
	}

	// Construct a new AES-GCM barrier
	barrier, err := NewAESGCMBarrier(conf.Physical)
	if err != nil {
		return nil, fmt.Errorf("barrier setup failed: %v", err)
	}

	// Make a default logger if not provided
	if conf.Logger == nil {
		conf.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	// Setup the core
	c := &Core{
		ha:            haBackend,
		advertiseAddr: conf.AdvertiseAddr,
		physical:      conf.Physical,
		barrier:       barrier,
		router:        NewRouter(),
		sealed:        true,
		standby:       true,
		logger:        conf.Logger,
	}

	// Setup the backends
	logicalBackends := make(map[string]logical.Factory)
	for k, f := range conf.LogicalBackends {
		logicalBackends[k] = f
	}
	logicalBackends["generic"] = PassthroughBackendFactory
	logicalBackends["system"] = func(map[string]string) (logical.Backend, error) {
		return NewSystemBackend(c), nil
	}
	c.logicalBackends = logicalBackends

	credentialBackends := make(map[string]logical.Factory)
	for k, f := range conf.CredentialBackends {
		credentialBackends[k] = f
	}
	credentialBackends["token"] = func(map[string]string) (logical.Backend, error) {
		return NewTokenStore(c)
	}
	c.credentialBackends = credentialBackends

	auditBackends := make(map[string]audit.Factory)
	for k, f := range conf.AuditBackends {
		auditBackends[k] = f
	}
	c.auditBackends = auditBackends
	return c, nil
}

// HandleRequest is used to handle a new incoming request
func (c *Core) HandleRequest(req *logical.Request) (*logical.Response, error) {
	c.stateLock.RLock()
	defer c.stateLock.RUnlock()
	if c.sealed {
		return nil, ErrSealed
	}
	if c.standby {
		return nil, ErrStandby
	}

	if c.router.LoginPath(req.Path) {
		return c.handleLoginRequest(req)
	} else {
		return c.handleRequest(req)
	}
}

func (c *Core) handleRequest(req *logical.Request) (*logical.Response, error) {
	defer metrics.MeasureSince([]string{"core", "handle_request"}, time.Now())
	// Validate the token
	auth, err := c.checkToken(req.Operation, req.Path, req.ClientToken)
	if err != nil {
		// If it is an internal error we return that, otherwise we
		// return invalid request so that the status codes can be correct
		var errType error
		switch err {
		case ErrInternalError, logical.ErrPermissionDenied:
			errType = err
		default:
			errType = logical.ErrInvalidRequest
		}

		return logical.ErrorResponse(err.Error()), errType
	}

	// Attach the display name
	req.DisplayName = auth.DisplayName

	// Create an audit trail of the request
	if err := c.auditBroker.LogRequest(auth, req); err != nil {
		c.logger.Printf("[ERR] core: failed to audit request (%#v): %v",
			req, err)
		return nil, ErrInternalError
	}

	// Route the request
	resp, err := c.router.Route(req)

	// If there is a secret, we must register it with the expiration manager.
	if resp != nil && resp.Secret != nil {
		// Apply the default lease if none given
		if resp.Secret.Lease == 0 {
			resp.Secret.Lease = defaultLeaseDuration
		}

		// Limit the lease duration
		if resp.Secret.Lease > maxLeaseDuration {
			resp.Secret.Lease = maxLeaseDuration
		}

		// Register the lease
		leaseID, err := c.expiration.Register(req, resp)
		if err != nil {
			c.logger.Printf(
				"[ERR] core: failed to register lease "+
					"(request: %#v, response: %#v): %v", req, resp, err)
			return nil, ErrInternalError
		}
		resp.Secret.LeaseID = leaseID
	}

	// Only the token store is allowed to return an auth block, for any
	// other request this is an internal error
	if resp != nil && resp.Auth != nil {
		if !strings.HasPrefix(req.Path, "auth/token/") {
			c.logger.Printf(
				"[ERR] core: unexpected Auth response for non-token backend "+
					"(request: %#v, response: %#v)", req, resp)
			return nil, ErrInternalError
		}

		// Set the default lease if non-provided, root tokens are exempt
		if resp.Auth.Lease == 0 && !strListContains(resp.Auth.Policies, "root") {
			resp.Auth.Lease = defaultLeaseDuration
		}

		// Limit the lease duration
		if resp.Auth.Lease > maxLeaseDuration {
			resp.Auth.Lease = maxLeaseDuration
		}

		// Register with the expiration manager
		if err := c.expiration.RegisterAuth(req.Path, resp.Auth); err != nil {
			c.logger.Printf("[ERR] core: failed to register token lease "+
				"(request: %#v, response: %#v): %v", req, resp, err)
			return nil, ErrInternalError
		}
	}

	// Create an audit trail of the response
	if err := c.auditBroker.LogResponse(auth, req, resp, err); err != nil {
		c.logger.Printf("[ERR] core: failed to audit response (request: %#v, response: %#v): %v",
			req, resp, err)
		return nil, ErrInternalError
	}

	// Return the response and error
	return resp, err
}

// handleLoginRequest is used to handle a login request, which is an
// unauthenticated request to the backend.
func (c *Core) handleLoginRequest(req *logical.Request) (*logical.Response, error) {
	defer metrics.MeasureSince([]string{"core", "handle_login_request"}, time.Now())

	// Create an audit trail of the request, auth is not available on login requests
	if err := c.auditBroker.LogRequest(nil, req); err != nil {
		c.logger.Printf("[ERR] core: failed to audit request (%#v): %v",
			req, err)
		return nil, ErrInternalError
	}

	// Route the request
	resp, err := c.router.Route(req)

	// If the response generated an authentication, then generate the token
	var auth *logical.Auth
	if resp != nil && resp.Auth != nil {
		auth = resp.Auth

		// Determine the source of the login
		source := c.router.MatchingMount(req.Path)
		source = strings.TrimPrefix(source, credentialRoutePrefix)
		source = strings.Replace(source, "/", "-", -1)

		// Prepend the source to the display name
		auth.DisplayName = strings.TrimSuffix(source+auth.DisplayName, "-")

		// Generate a token
		te := TokenEntry{
			Path:        req.Path,
			Policies:    auth.Policies,
			Meta:        auth.Metadata,
			DisplayName: auth.DisplayName,
		}
		if err := c.tokenStore.Create(&te); err != nil {
			c.logger.Printf("[ERR] core: failed to create token: %v", err)
			return nil, ErrInternalError
		}

		// Populate the client token
		resp.Auth.ClientToken = te.ID

		// Set the default lease if non-provided, root tokens are exempt
		if auth.Lease == 0 && !strListContains(auth.Policies, "root") {
			auth.Lease = defaultLeaseDuration
		}

		// Limit the lease duration
		if resp.Auth.Lease > maxLeaseDuration {
			resp.Auth.Lease = maxLeaseDuration
		}

		// Register with the expiration manager
		if err := c.expiration.RegisterAuth(req.Path, auth); err != nil {
			c.logger.Printf("[ERR] core: failed to register token lease "+
				"(request: %#v, response: %#v): %v", req, resp, err)
			return nil, ErrInternalError
		}

		// Attach the display name, might be used by audit backends
		req.DisplayName = auth.DisplayName
	}

	// Create an audit trail of the response
	if err := c.auditBroker.LogResponse(auth, req, resp, err); err != nil {
		c.logger.Printf("[ERR] core: failed to audit response (request: %#v, response: %#v): %v",
			req, resp, err)
		return nil, ErrInternalError
	}

	return resp, err
}

func (c *Core) checkToken(
	op logical.Operation, path string, token string) (*logical.Auth, error) {
	defer metrics.MeasureSince([]string{"core", "check_token"}, time.Now())

	// Ensure there is a client token
	if token == "" {
		return nil, fmt.Errorf("missing client token")
	}

	// Resolve the token policy
	te, err := c.tokenStore.Lookup(token)
	if err != nil {
		c.logger.Printf("[ERR] core: failed to lookup token: %v", err)
		return nil, ErrInternalError
	}

	// Ensure the token is valid
	if te == nil {
		return nil, logical.ErrPermissionDenied
	}

	// Attempt to use the token
	if err := c.tokenStore.UseToken(te); err != nil {
		c.logger.Printf("[ERR] core: failed to use token: %v", err)
		return nil, ErrInternalError
	}

	// Construct the corresponding ACL object
	acl, err := c.policy.ACL(te.Policies...)
	if err != nil {
		c.logger.Printf("[ERR] core: failed to construct ACL: %v", err)
		return nil, ErrInternalError
	}

	// Check if this is a root protected path
	if c.router.RootPath(path) && !acl.RootPrivilege(path) {
		return nil, logical.ErrPermissionDenied
	}

	// Check the standard non-root ACLs
	if !acl.AllowOperation(op, path) {
		return nil, logical.ErrPermissionDenied
	}

	// Create the auth response
	auth := &logical.Auth{
		ClientToken: token,
		Policies:    te.Policies,
		Metadata:    te.Meta,
		DisplayName: te.DisplayName,
	}
	return auth, nil
}

// Initialized checks if the Vault is already initialized
func (c *Core) Initialized() (bool, error) {
	// Check the barrier first
	init, err := c.barrier.Initialized()
	if err != nil {
		c.logger.Printf("[ERR] core: barrier init check failed: %v", err)
		return false, err
	}
	if !init {
		return false, nil
	}
	if !init {
		c.logger.Printf("[INFO] core: security barrier not initialized")
		return false, nil
	}

	// Verify the seal configuration
	sealConf, err := c.SealConfig()
	if err != nil {
		return false, err
	}
	if sealConf == nil {
		return false, nil
	}
	return true, nil
}

// Initialize is used to initialize the Vault with the given
// configurations.
func (c *Core) Initialize(config *SealConfig) (*InitResult, error) {
	// Check if the seal configuraiton is valid
	if err := config.Validate(); err != nil {
		c.logger.Printf("[ERR] core: invalid seal configuration: %v", err)
		return nil, fmt.Errorf("invalid seal configuration: %v", err)
	}

	// Avoid an initialization race
	c.stateLock.Lock()
	defer c.stateLock.Unlock()

	// Check if we are initialized
	init, err := c.Initialized()
	if err != nil {
		return nil, err
	}
	if init {
		return nil, ErrAlreadyInit
	}

	// Encode the seal configuration
	buf, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to encode seal configuration: %v", err)
	}

	// Store the seal configuration
	pe := &physical.Entry{
		Key:   coreSealConfigPath,
		Value: buf,
	}
	if err := c.physical.Put(pe); err != nil {
		c.logger.Printf("[ERR] core: failed to read seal configuration: %v", err)
		return nil, fmt.Errorf("failed to check seal configuration: %v", err)
	}

	// Generate a master key
	masterKey, err := c.barrier.GenerateKey()
	if err != nil {
		c.logger.Printf("[ERR] core: failed to generate master key: %v", err)
		return nil, fmt.Errorf("master key generation failed: %v", err)
	}

	// Initialize the barrier
	if err := c.barrier.Initialize(masterKey); err != nil {
		c.logger.Printf("[ERR] core: failed to initialize barrier: %v", err)
		return nil, fmt.Errorf("failed to initialize barrier: %v", err)
	}

	// Return the master key if only a single key part is used
	results := new(InitResult)
	if config.SecretShares == 1 {
		results.SecretShares = append(results.SecretShares, masterKey)

	} else {
		// Split the master key using the Shamir algorithm
		shares, err := shamir.Split(masterKey, config.SecretShares, config.SecretThreshold)
		if err != nil {
			c.logger.Printf("[ERR] core: failed to generate shares: %v", err)
			return nil, fmt.Errorf("failed to generate shares: %v", err)
		}
		results.SecretShares = shares
	}
	c.logger.Printf("[INFO] core: security barrier initialized")

	// Unseal the barrier
	if err := c.barrier.Unseal(masterKey); err != nil {
		c.logger.Printf("[ERR] core: failed to unseal barrier: %v", err)
		return nil, fmt.Errorf("failed to unseal barrier: %v", err)
	}

	// Ensure the barrier is re-sealed
	defer func() {
		if err := c.barrier.Seal(); err != nil {
			c.logger.Printf("[ERR] core: failed to seal barrier: %v", err)
		}
	}()

	// Perform initial setup
	if err := c.postUnseal(); err != nil {
		c.logger.Printf("[ERR] core: post-unseal setup failed: %v", err)
		return nil, err
	}

	// Generate a new root token
	rootToken, err := c.tokenStore.RootToken()
	if err != nil {
		c.logger.Printf("[ERR] core: root token generation failed: %v", err)
		return nil, err
	}
	results.RootToken = rootToken.ID
	c.logger.Printf("[INFO] core: root token generated")

	// Prepare to re-seal
	if err := c.preSeal(); err != nil {
		c.logger.Printf("[ERR] core: pre-seal teardown failed: %v", err)
		return nil, err
	}
	return results, nil
}

// Sealed checks if the Vault is current sealed
func (c *Core) Sealed() (bool, error) {
	c.stateLock.RLock()
	defer c.stateLock.RUnlock()
	return c.sealed, nil
}

// Standby checks if the Vault is in standby mode
func (c *Core) Standby() (bool, error) {
	c.stateLock.RLock()
	defer c.stateLock.RUnlock()
	return c.standby, nil
}

// Leader is used to get the current active leader
func (c *Core) Leader() (bool, string, error) {
	c.stateLock.RLock()
	defer c.stateLock.RUnlock()
	// Check if HA enabled
	if c.ha == nil {
		return false, "", ErrHANotEnabled
	}

	// Check if sealed
	if c.sealed {
		return false, "", ErrSealed
	}

	// Check if we are the leader
	if !c.standby {
		return true, c.advertiseAddr, nil
	}

	// Initialize a lock
	lock, err := c.ha.LockWith(coreLockPath, "read")
	if err != nil {
		return false, "", err
	}

	// Read the value
	held, value, err := lock.Value()
	if err != nil {
		return false, "", err
	}
	if !held {
		return false, "", nil
	}

	// Value is the UUID of the leader, fetch the key
	key := coreLeaderPrefix + value
	entry, err := c.barrier.Get(key)
	if err != nil {
		return false, "", err
	}
	if entry == nil {
		return false, "", nil
	}

	// Leader address is in the entry
	return false, string(entry.Value), nil
}

// SealConfiguration is used to return information
// about the configuration of the Vault and it's current
// status.
func (c *Core) SealConfig() (*SealConfig, error) {
	// Fetch the core configuration
	pe, err := c.physical.Get(coreSealConfigPath)
	if err != nil {
		c.logger.Printf("[ERR] core: failed to read seal configuration: %v", err)
		return nil, fmt.Errorf("failed to check seal configuration: %v", err)
	}

	// If the seal configuration is missing, we are not initialized
	if pe == nil {
		c.logger.Printf("[INFO] core: seal configuration missing, not initialized")
		return nil, nil
	}

	// Decode the barrier entry
	var conf SealConfig
	if err := json.Unmarshal(pe.Value, &conf); err != nil {
		c.logger.Printf("[ERR] core: failed to decode seal configuration: %v", err)
		return nil, fmt.Errorf("failed to decode seal configuration: %v", err)
	}

	// Check for a valid seal configuration
	if err := conf.Validate(); err != nil {
		c.logger.Printf("[ERR] core: invalid seal configuration: %v", err)
		return nil, fmt.Errorf("seal validation failed: %v", err)
	}
	return &conf, nil
}

// SecretProgress returns the number of keys provided so far
func (c *Core) SecretProgress() int {
	c.stateLock.RLock()
	defer c.stateLock.RUnlock()
	return len(c.unlockParts)
}

// Unseal is used to provide one of the key parts to unseal the Vault.
//
// They key given as a parameter will automatically be zerod after
// this method is done with it. If you want to keep the key around, a copy
// should be made.
func (c *Core) Unseal(key []byte) (bool, error) {
	defer metrics.MeasureSince([]string{"core", "unseal"}, time.Now())

	// Verify the key length
	min, max := c.barrier.KeyLength()
	max += shamir.ShareOverhead
	if len(key) < min {
		return false, &ErrInvalidKey{fmt.Sprintf("key is shorter than minimum %d bytes", min)}
	}
	if len(key) > max {
		return false, &ErrInvalidKey{fmt.Sprintf("key is longer than maximum %d bytes", max)}
	}

	// Get the seal configuration
	config, err := c.SealConfig()
	if err != nil {
		return false, err
	}

	// Ensure the barrier is initialized
	if config == nil {
		return false, ErrNotInit
	}

	c.stateLock.Lock()
	defer c.stateLock.Unlock()

	// Check if already unsealed
	if !c.sealed {
		return true, nil
	}

	// Check if we already have this piece
	for _, existing := range c.unlockParts {
		if bytes.Equal(existing, key) {
			return false, nil
		}
	}

	// Store this key
	c.unlockParts = append(c.unlockParts, key)

	// Check if we don't have enough keys to unlock
	if len(c.unlockParts) < config.SecretThreshold {
		c.logger.Printf("[DEBUG] core: cannot unseal, have %d of %d keys",
			len(c.unlockParts), config.SecretThreshold)
		return false, nil
	}

	// Recover the master key
	var masterKey []byte
	if config.SecretThreshold == 1 {
		masterKey = c.unlockParts[0]
		c.unlockParts = nil
	} else {
		masterKey, err = shamir.Combine(c.unlockParts)
		c.unlockParts = nil
		if err != nil {
			return false, fmt.Errorf("failed to compute master key: %v", err)
		}
	}
	defer memzero(masterKey)

	// Attempt to unlock
	if err := c.barrier.Unseal(masterKey); err != nil {
		return false, err
	}
	c.logger.Printf("[INFO] core: vault is unsealed")

	// Do post-unseal setup if HA is not enabled
	if c.ha == nil {
		c.standby = false
		if err := c.postUnseal(); err != nil {
			c.logger.Printf("[ERR] core: post-unseal setup failed: %v", err)
			c.barrier.Seal()
			c.logger.Printf("[WARN] core: vault is sealed")
			return false, err
		}
	} else {
		// Go to standby mode, wait until we are active to unseal
		c.standbyDoneCh = make(chan struct{})
		c.standbyStopCh = make(chan struct{})
		go c.runStandby(c.standbyDoneCh, c.standbyStopCh)
	}

	// Success!
	c.sealed = false
	return true, nil
}

// Seal is used to re-seal the Vault. This requires the Vault to
// be unsealed again to perform any further operations.
func (c *Core) Seal(token string) error {
	defer metrics.MeasureSince([]string{"core", "seal"}, time.Now())
	c.stateLock.Lock()
	defer c.stateLock.Unlock()
	if c.sealed {
		return nil
	}

	// Validate the token is a root token
	_, err := c.checkToken(logical.WriteOperation, "sys/seal", token)
	if err != nil {
		return err
	}

	// Enable that we are sealed to prevent furthur transactions
	c.sealed = true

	// Do pre-seal teardown if HA is not enabled
	if c.ha == nil {
		if err := c.preSeal(); err != nil {
			c.logger.Printf("[ERR] core: pre-seal teardown failed: %v", err)
			return fmt.Errorf("internal error")
		}
	} else {
		// Signal the standby goroutine to shutdown, wait for completion
		close(c.standbyStopCh)

		// Release the lock while we wait to avoid deadlocking
		c.stateLock.Unlock()
		<-c.standbyDoneCh
		c.stateLock.Lock()
	}

	if err := c.barrier.Seal(); err != nil {
		return err
	}
	c.logger.Printf("[INFO] core: vault is sealed")
	return nil
}

// postUnseal is invoked after the barrier is unsealed, but before
// allowing any user operations. This allows us to setup any state that
// requires the Vault to be unsealed such as mount tables, logical backends,
// credential stores, etc.
func (c *Core) postUnseal() error {
	defer metrics.MeasureSince([]string{"core", "post_unseal"}, time.Now())
	c.logger.Printf("[INFO] core: post-unseal setup starting")
	if cache, ok := c.physical.(*physical.Cache); ok {
		cache.Purge()
	}
	if err := c.loadMounts(); err != nil {
		return err
	}
	if err := c.setupMounts(); err != nil {
		return err
	}
	if err := c.startRollback(); err != nil {
		return err
	}
	if err := c.setupPolicyStore(); err != nil {
		return nil
	}
	if err := c.loadCredentials(); err != nil {
		return nil
	}
	if err := c.setupCredentials(); err != nil {
		return nil
	}
	if err := c.setupExpiration(); err != nil {
		return err
	}
	if err := c.loadAudits(); err != nil {
		return err
	}
	if err := c.setupAudits(); err != nil {
		return err
	}
	c.metricsCh = make(chan struct{})
	go c.emitMetrics(c.metricsCh)
	c.logger.Printf("[INFO] core: post-unseal setup complete")
	return nil
}

// preSeal is invoked before the barrier is sealed, allowing
// for any state teardown required.
func (c *Core) preSeal() error {
	defer metrics.MeasureSince([]string{"core", "pre_seal"}, time.Now())
	c.logger.Printf("[INFO] core: pre-seal teardown starting")
	if c.metricsCh != nil {
		close(c.metricsCh)
		c.metricsCh = nil
	}
	if err := c.teardownAudits(); err != nil {
		return err
	}
	if err := c.stopExpiration(); err != nil {
		return err
	}
	if err := c.teardownCredentials(); err != nil {
		return err
	}
	if err := c.teardownPolicyStore(); err != nil {
		return err
	}
	if err := c.stopRollback(); err != nil {
		return err
	}
	if err := c.unloadMounts(); err != nil {
		return err
	}
	if cache, ok := c.physical.(*physical.Cache); ok {
		cache.Purge()
	}
	c.logger.Printf("[INFO] core: pre-seal teardown complete")
	return nil
}

// runStandby is a long running routine that is used when an HA backend
// is enabled. It waits until we are leader and switches this Vault to
// active.
func (c *Core) runStandby(doneCh, stopCh chan struct{}) {
	defer close(doneCh)
	c.logger.Printf("[INFO] core: entering standby mode")
	for {
		// Check for a shutdown
		select {
		case <-stopCh:
			return
		default:
		}

		// Create a lock
		uuid := generateUUID()
		lock, err := c.ha.LockWith(coreLockPath, uuid)
		if err != nil {
			c.logger.Printf("[ERR] core: failed to create lock: %v", err)
			return
		}

		// Attempt the acquisition
		leaderCh := c.acquireLock(lock, stopCh)

		// Bail if we are being shutdown
		if leaderCh == nil {
			return
		}
		c.logger.Printf("[INFO] core: acquired lock, enabling active operation")

		// Advertise ourself as leader
		if err := c.advertiseLeader(uuid); err != nil {
			c.logger.Printf("[ERR] core: leader advertisement setup failed: %v", err)
			lock.Unlock()
			continue
		}

		// Attempt the post-unseal process
		c.stateLock.Lock()
		err = c.postUnseal()
		if err == nil {
			c.standby = false
		}
		c.stateLock.Unlock()

		// Handle a failure to unseal
		if err != nil {
			c.logger.Printf("[ERR] core: post-unseal setup failed: %v", err)
			lock.Unlock()
			continue
		}

		// Monitor a loss of leadership
		select {
		case <-leaderCh:
			c.logger.Printf("[WARN] core: leadership lost, stopping active operation")
		case <-stopCh:
			c.logger.Printf("[WARN] core: stopping active operation")
		}

		// Clear ourself as leader
		if err := c.clearLeader(uuid); err != nil {
			c.logger.Printf("[ERR] core: clearing leader advertisement failed: %v", err)
		}

		// Attempt the pre-seal process
		c.stateLock.Lock()
		c.standby = true
		err = c.preSeal()
		c.stateLock.Unlock()

		// Give up leadership
		lock.Unlock()

		// Check for a failure to prepare to seal
		if err := c.preSeal(); err != nil {
			c.logger.Printf("[ERR] core: pre-seal teardown failed: %v", err)
			continue
		}
	}
}

// acquireLock blocks until the lock is acquired, returning the leaderCh
func (c *Core) acquireLock(lock physical.Lock, stopCh <-chan struct{}) <-chan struct{} {
	for {
		// Attempt lock acquisition
		leaderCh, err := lock.Lock(stopCh)
		if err == nil {
			return leaderCh
		}

		// Retry the acquisition
		c.logger.Printf("[ERR] core: failed to acquire lock: %v", err)
		select {
		case <-time.After(lockRetryInterval):
		case <-stopCh:
			return nil
		}
	}
}

// advertiseLeader is used to advertise the current node as leader
func (c *Core) advertiseLeader(uuid string) error {
	ent := &Entry{
		Key:   coreLeaderPrefix + uuid,
		Value: []byte(c.advertiseAddr),
	}
	return c.barrier.Put(ent)
}

// clearLeader is used to clear our leadership entry
func (c *Core) clearLeader(uuid string) error {
	key := coreLeaderPrefix + uuid
	return c.barrier.Delete(key)
}

// emitMetrics is used to periodically expose metrics while runnig
func (c *Core) emitMetrics(stopCh chan struct{}) {
	for {
		select {
		case <-time.After(time.Second):
			c.expiration.emitMetrics()
		case <-stopCh:
			return
		}
	}
}
