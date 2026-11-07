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
)

// clientName is what a client can be called.
var clientName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

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

	mu      sync.Mutex
	records map[string]clientRecord // name -> record
	byHash  map[string]string       // hash -> name
}

func loadClients(values *store.Namespace) (*clients, error) {
	c := &clients{values: values, now: time.Now, records: map[string]clientRecord{}, byHash: map[string]string{}}
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
		c.records[name], c.byHash[rec.Hash] = rec, name
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
	if !clientName.MatchString(name) {
		return "", fmt.Errorf("%q can't name a client: use lower-case letters, digits, '.', '_' and '-'", name)
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
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
	c.records[name], c.byHash[rec.Hash] = rec, name
	return token, nil
}

// revoke removes a client, so its token stops working at once.
func (c *clients) revoke(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.records[name]
	if !ok {
		return fmt.Errorf("there's no client called %s", name)
	}
	// Gone from memory first: a token that's being revoked shouldn't work while it's written.
	delete(c.records, name)
	delete(c.byHash, rec.Hash)
	if err := c.values.Delete(apiTokenPrefix + name); err != nil {
		return fmt.Errorf("revoking client %s: %w", name, err)
	}
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

// check says whose token this is. A token is a hash lookup, so its time says nothing about others.
func (c *clients) check(token string) (string, bool) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return "", false
	}
	hash := hashToken(token)
	c.mu.Lock()
	defer c.mu.Unlock()
	name, ok := c.byHash[hash]
	if !ok {
		return "", false
	}
	rec := c.records[name]
	if now := c.now().UTC(); now.Sub(rec.LastUsed) >= lastUseEvery {
		rec.LastUsed = now
		c.records[name] = rec
		// Worth keeping, not worth refusing a call over.
		_ = c.save(name, rec)
	}
	return name, true
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
