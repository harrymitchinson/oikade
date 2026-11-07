package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/store"
)

const (
	// apiNamespace is where the store keeps the API's identity and its clients' tokens.
	apiNamespace   = "api"
	apiIdentityKey = "identity"
	apiTokenPrefix = "token/"
	// DefaultAPIPort is the network API's port when an address has none.
	DefaultAPIPort = "8443"
	// lastUseEvery bounds how often a client's last use is written, so calls aren't store writes.
	lastUseEvery = time.Minute
	// tokenPrefix starts every token, so a secret scanner can tell one.
	tokenPrefix = "oik_"
	// codeLifetime is how long a login code works. It works once, and a restart ends it.
	codeLifetime = 10 * time.Minute
)

// errBadCode is a login with a code that's wrong, used or expired. It doesn't say which.
var errBadCode = fmt.Errorf("that code doesn't work: it's wrong, used or more than %v old. Run oikade client add again",
	codeLifetime)

// clientName is what a client can be called.
var clientName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func validClientName(name string) error {
	if !clientName.MatchString(name) {
		return fmt.Errorf("%q can't name a client: use lower-case letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

// APIAddress fills in the port of an address that has none.
func APIAddress(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), DefaultAPIPort)
}

// apiIdentity is the network API's key and self-signed certificate. Clients pin its public key, so
// its names and expiry don't matter.
type apiIdentity struct {
	// Key is a PKCS #8 P-256 private key, and Cert the certificate's DER.
	Key  []byte `json:"key"`
	Cert []byte `json:"cert"`
}

// loadAPIIdentity reads the API's identity, or makes one if it never had one. An unreadable one is an
// error rather than a new one, which would make every client's pin wrong.
func loadAPIIdentity(values *store.Namespace) (tls.Certificate, string, error) {
	var id apiIdentity
	raw, ok, err := values.Get(apiIdentityKey)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("reading the API's identity: %w", err)
	}
	if ok {
		if err := json.Unmarshal(raw, &id); err != nil {
			return tls.Certificate{}, "", fmt.Errorf("reading the API's identity: %w", err)
		}
	} else {
		if id, err = newAPIIdentity(); err != nil {
			return tls.Certificate{}, "", err
		}
		if raw, err = json.Marshal(id); err != nil {
			return tls.Certificate{}, "", err
		}
		if err := values.Put(apiIdentityKey, raw); err != nil {
			return tls.Certificate{}, "", fmt.Errorf("saving the API's identity: %w", err)
		}
	}
	key, err := x509.ParsePKCS8PrivateKey(id.Key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("reading the API's key: %w", err)
	}
	leaf, err := x509.ParseCertificate(id.Cert)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("reading the API's certificate: %w", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{id.Cert}, PrivateKey: key, Leaf: leaf}
	return cert, api.Pin(leaf.RawSubjectPublicKeyInfo), nil
}

func newAPIIdentity() (apiIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return apiIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return apiIdentity{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "oikade"},
		NotBefore:    now.Add(-time.Hour),
		// Pinned, so it never needs to expire.
		NotAfter:    now.AddDate(100, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return apiIdentity{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return apiIdentity{}, err
	}
	return apiIdentity{Key: der, Cert: cert}, nil
}

// clientRecord is what the store keeps for a client: never its token, only the token's hash.
type clientRecord struct {
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
}

// clients are the tokens the network API takes.
type clients struct {
	values *store.Namespace
	now    func() time.Time

	// disk orders writes to the store, so a last use can't write back a client a revoke deleted. It's
	// taken before mu.
	disk    sync.Mutex
	mu      sync.Mutex
	records map[string]clientRecord // name -> record
	byHash  map[string]string       // hash -> name
	// revoked closes when the client is revoked, which ends its streams.
	revoked map[string]chan struct{}
	// codes are login codes waiting to be used, by hash. They're only in memory.
	codes map[string]pendingLogin
}

type pendingLogin struct {
	name    string
	expires time.Time
}

func loadClients(values *store.Namespace) (*clients, error) {
	c := &clients{
		values: values, now: time.Now,
		records: map[string]clientRecord{}, byHash: map[string]string{}, revoked: map[string]chan struct{}{},
		codes: map[string]pendingLogin{},
	}
	keys, err := values.Keys()
	if err != nil {
		return nil, fmt.Errorf("reading the API's clients: %w", err)
	}
	for _, k := range keys {
		name, ok := strings.CutPrefix(k, apiTokenPrefix)
		if !ok {
			continue
		}
		raw, _, err := values.Get(k)
		if err != nil {
			return nil, fmt.Errorf("reading client %s: %w", name, err)
		}
		var rec clientRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("reading client %s: %w", name, err)
		}
		c.records[name], c.byHash[rec.Hash], c.revoked[name] = rec, name, make(chan struct{})
	}
	return c, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (c *clients) save(name string, rec clientRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return c.values.Put(apiTokenPrefix+name, raw)
}

// add makes a client and returns its token, which is never seen again.
func (c *clients) add(name string) (string, error) {
	if err := validClientName(name); err != nil {
		return "", err
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	c.disk.Lock()
	defer c.disk.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.records[name]; taken {
		return "", fmt.Errorf("there's already a client called %s: revoke it first", name)
	}
	rec := clientRecord{Hash: hashToken(token), Created: c.now().UTC()}
	// On disk before it's handed out, so a token that works survives a crash.
	if err := c.save(name, rec); err != nil {
		return "", fmt.Errorf("saving client %s: %w", name, err)
	}
	c.records[name], c.byHash[rec.Hash], c.revoked[name] = rec, name, make(chan struct{})
	return token, nil
}

// issue makes a code that a login trades for a token for a new client, once, within codeLifetime.
// A new code for the same name replaces the last.
func (c *clients) issue(name string) (string, time.Time, error) {
	if err := validClientName(name); err != nil {
		return "", time.Time{}, err
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	code := base64.RawURLEncoding.EncodeToString(secret)
	expires := c.now().UTC().Add(codeLifetime)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.records[name]; taken {
		return "", time.Time{}, fmt.Errorf("there's already a client called %s: revoke it first", name)
	}
	for hash, p := range c.codes {
		if p.name == name || !c.now().Before(p.expires) {
			delete(c.codes, hash)
		}
	}
	c.codes[hashToken(code)] = pendingLogin{name: name, expires: expires}
	return code, expires, nil
}

// redeem trades a code for a client and its token. A code goes on its first try, right or not.
func (c *clients) redeem(code string) (string, string, error) {
	hash := hashToken(code)
	c.mu.Lock()
	p, ok := c.codes[hash]
	delete(c.codes, hash)
	c.mu.Unlock()
	if !ok || !c.now().Before(p.expires) {
		return "", "", errBadCode
	}
	token, err := c.add(p.name)
	return p.name, token, err
}

// revoke removes a client, so its token stops working at once, and its streams end. It's off disk
// before it's off the list, so a revoke that failed can be tried again, and one that worked can't
// come back with a restart.
func (c *clients) revoke(name string) error {
	c.disk.Lock()
	defer c.disk.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.records[name]
	if !ok {
		return fmt.Errorf("there's no client called %s", name)
	}
	if err := c.values.Delete(apiTokenPrefix + name); err != nil {
		return fmt.Errorf("revoking client %s: %w", name, err)
	}
	delete(c.records, name)
	delete(c.byHash, rec.Hash)
	close(c.revoked[name])
	delete(c.revoked, name)
	return nil
}

func (c *clients) list() []api.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]api.Client, 0, len(c.records))
	for name, rec := range c.records {
		out = append(out, api.Client{Name: name, Created: rec.Created, LastUsed: rec.LastUsed})
	}
	slices.SortFunc(out, func(a, b api.Client) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// check says whose token this is, and returns what closes when it's revoked. A token is a hash
// lookup, so its time says nothing about others.
func (c *clients) check(token string) (string, <-chan struct{}, bool) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return "", nil, false
	}
	hash := hashToken(token)
	c.mu.Lock()
	name, ok := c.byHash[hash]
	if !ok {
		c.mu.Unlock()
		return "", nil, false
	}
	rec, revoked := c.records[name], c.revoked[name]
	now := c.now().UTC()
	due := now.Sub(rec.LastUsed) >= lastUseEvery
	if due {
		rec.LastUsed = now
		c.records[name] = rec
	}
	c.mu.Unlock()
	if due {
		// Outside the lock, as it waits for the disk. Worth keeping, not worth refusing a call over, and
		// a revoke meanwhile wins: a record it deleted isn't written back.
		c.saveIfKnown(name, rec)
	}
	return name, revoked, true
}

// saveIfKnown writes a last use, unless the client has gone since. Only disk is held while it
// writes, so other calls' checks don't wait.
func (c *clients) saveIfKnown(name string, rec clientRecord) {
	c.disk.Lock()
	defer c.disk.Unlock()
	c.mu.Lock()
	current, ok := c.records[name]
	c.mu.Unlock()
	if ok && current.Hash == rec.Hash {
		_ = c.save(name, rec)
	}
}

// caller is who made a call: a client by name over the network, or anyone on the socket.
type caller struct {
	name  string
	local bool
}

type callerKey struct{}

func withCaller(ctx context.Context, c caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func callerOf(ctx context.Context) caller {
	c, _ := ctx.Value(callerKey{}).(caller)
	return c
}

// localCaller is the socket's caller in logs.
var localCaller = caller{name: "local", local: true}

// errNotLocal is a call only the host's machine may make.
var errNotLocal = errors.New("only the host's machine can do this, over the socket: run it there")

func newAuthFailures() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oikade_api_auth_failures_total",
		Help: "Calls to the network API refused for want of a valid token.",
	})
}
