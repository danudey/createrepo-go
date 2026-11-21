package gpg

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Audit checks each signature a run makes, right after it is made, and
// collects what could stop the repository's users from verifying it.
//
// Signing succeeding proves only that the signer had a key. Whether a user
// can verify the result depends on the public key they install, which is
// published separately and can fall out of step: gpg signs with a key's
// signing subkey whenever it has one, so a public key exported before that
// subkey was added (or one stripped of subkeys) verifies nothing, and nothing
// fails until a user's dnf or apt refuses the repository.
//
// An Audit therefore confirms that every signature was made by the configured
// key or one of its subkeys, failing the signing when it was not, and records
// as warnings for Report: signatures made by a subkey; signatures the public
// key at the repository's key URL does not verify; a key about to expire; and
// a published key file that is served insecurely or holds private key
// material.
//
// An Audit is safe for concurrent use. A nil *Audit checks nothing.
type Audit struct {
	keyFile, keyID, keyURL string

	// HTTPClient fetches the public key from the key URL; nil uses a client
	// with a 30-second timeout.
	HTTPClient *http.Client
	// Now is the clock that signatures and expiry dates are judged by; nil
	// means time.Now.
	Now func() time.Time
	// ExpiryWarning is how far ahead a key's expiry is warned about; zero
	// means 30 days.
	ExpiryWarning time.Duration

	once      sync.Once
	expected  string          // the configured key's primary fingerprint
	local     *openpgp.Entity // the configured key, public parts used
	published openpgp.EntityList

	mu       sync.Mutex
	items    int            // documents and packages signed
	bySigner map[string]int // issuer long key id -> items it signed
	failures map[string]*publishedFailure
	notes    []string
	seen     map[string]bool
}

// publishedFailure counts the items the published key did not verify for one
// signer, and why.
type publishedFailure struct {
	n      int
	reason string
}

// NewAudit returns an Audit for signatures made with the key given as a
// private key file (keyFile) or a keyring key id (keyID), checked against the
// public key published at keyURL (which may be empty). Nothing is read or
// fetched until the first signature is checked.
func NewAudit(keyFile, keyID, keyURL string) *Audit {
	return &Audit{keyFile: keyFile, keyID: keyID, keyURL: keyURL}
}

// CheckKeyURL reports whether s can be fetched as a repository's public key
// URL: an absolute http or https URL. It is applied to a URL read from a
// repository's config file, which this tool does not necessarily control.
func CheckKeyURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("key URL %q: %w", s, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("key URL %q is not an http(s) URL", s)
	}
	return nil
}

// maxPublicKeySize bounds a fetched public key file; a real one is a few KiB.
const maxPublicKeySize = 4 << 20

// FetchPublicKey downloads and parses the OpenPGP key(s) published at rawURL.
func FetchPublicKey(ctx context.Context, client *http.Client, rawURL string) (openpgp.EntityList, error) {
	if err := CheckKeyURL(rawURL); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPublicKeySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPublicKeySize {
		return nil, fmt.Errorf("the file is larger than %d bytes, which no public key is", maxPublicKeySize)
	}
	keys, err := ReadKeyRing(data)
	if err != nil {
		return nil, fmt.Errorf("it is not an OpenPGP key: %w", err)
	}
	return keys, nil
}

// LoadPublicKeys reads the OpenPGP key(s) at rawURL: fetched over http(s), or
// read from disk for a file:// URL, as a .repo file's gpgkey= may name.
func LoadPublicKeys(ctx context.Context, client *http.Client, rawURL string) (openpgp.EntityList, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("key URL %q: %w", rawURL, err)
	}
	if u.Scheme != "file" {
		return FetchPublicKey(ctx, client, rawURL)
	}
	data, err := os.ReadFile(u.Path)
	if err != nil {
		return nil, err
	}
	keys, err := ReadKeyRing(data)
	if err != nil {
		return nil, fmt.Errorf("it is not an OpenPGP key: %w", err)
	}
	return keys, nil
}

func (a *Audit) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// load resolves the configured key and fetches the published one, once.
func (a *Audit) load(ctx context.Context) {
	a.once.Do(func() {
		a.loadLocal()
		a.loadPublished(ctx)
	})
}

func (a *Audit) loadLocal() {
	var keys openpgp.EntityList
	switch {
	case a.keyFile != "":
		data, err := os.ReadFile(a.keyFile)
		if err == nil {
			keys, err = ReadKeyRing(data)
		}
		if err != nil {
			a.note("could not read the signing key %s to confirm which key made each signature: %v", a.keyFile, err)
			return
		}
		// The signer uses the first entity holding a private key.
		for _, e := range keys {
			if e.PrivateKey != nil {
				a.local = e
				break
			}
		}
	case a.keyID != "":
		fpr, err := fingerprintFromKeyring(a.keyID)
		if err != nil {
			a.note("could not resolve the signing key %s to confirm which key made each signature: %v", a.keyID, err)
			return
		}
		a.expected = fpr
		armored, err := ExportPublicKey(a.keyID)
		if err == nil {
			keys, err = ReadKeyRing(armored)
		}
		if err != nil {
			a.note("could not export the signing key %s to confirm which key made each signature: %v", a.keyID, err)
			return
		}
		for _, e := range keys {
			if FingerprintOf(e) == fpr {
				a.local = e
				break
			}
		}
	default:
		return
	}
	if a.local == nil {
		a.note("the signing key could not be found, so which key made each signature could not be confirmed")
		return
	}
	a.expected = FingerprintOf(a.local)
}

func (a *Audit) loadPublished(ctx context.Context) {
	if a.keyURL == "" {
		return
	}
	if u, err := url.Parse(a.keyURL); err == nil && u.Scheme == "http" {
		a.note("the public key URL %s uses plain http: anyone who can tamper with the connection can hand your users a different key. Serve it over https", a.keyURL)
	}
	keys, err := FetchPublicKey(ctx, a.HTTPClient, a.keyURL)
	if err != nil {
		a.note("could not fetch the public key at %s (%v), so the signatures were not checked against the key your users install", a.keyURL, err)
		return
	}
	for _, e := range keys {
		secret := e.PrivateKey != nil
		for _, sk := range e.Subkeys {
			secret = secret || sk.PrivateKey != nil
		}
		if secret {
			a.note("the file at %s contains PRIVATE key material for %s: anyone can download it and sign as you. Replace it with the public key alone (gpg --armor --export %s) and revoke the key", a.keyURL, FingerprintOf(e), FingerprintOf(e))
		}
	}
	a.published = keys
}

// Keys returns the configured signing key and the published public key, for a
// caller checking signatures it reads itself (an rpm's). Either is nil when it
// could not be loaded, or, for the published key, when no key URL was given.
func (a *Audit) Keys(ctx context.Context) (local, published openpgp.EntityList) {
	if a == nil {
		return nil, nil
	}
	a.load(ctx)
	if a.local != nil {
		local = openpgp.EntityList{a.local}
	}
	return local, a.published
}

// Issued is one signature on a signed item, for Audit.Check: who made it, and
// what checking it against each key found.
type Issued struct {
	// KeyID is the issuer's long key id, as a signature packet carries it.
	KeyID string
	// LocalErr is why the signature does not verify against the configured
	// key itself; nil when it does, or when that key is not loaded.
	LocalErr error
	// PublishedErr is why the signature does not verify against the published
	// public key; nil when it does, or when there is no published key.
	PublishedErr error
	// Refused, when set, is why the target's clients refuse the signature
	// even though it verifies (a hash or key their crypto policy forbids).
	Refused string
}

// Detached checks an armored or binary detached signature, just made, over
// data. An error means it was not made by the configured key.
func (a *Audit) Detached(data, sig []byte) error {
	if a == nil {
		return nil
	}
	a.load(context.Background())
	return a.checkDetached(data, sig)
}

// Clearsigned checks an inline-signed document, just made. An error means it
// was not made by the configured key.
func (a *Audit) Clearsigned(document []byte) error {
	if a == nil {
		return nil
	}
	a.load(context.Background())
	block, _ := clearsign.Decode(document)
	if block == nil {
		return fmt.Errorf("the signed document is not an OpenPGP clearsigned message")
	}
	sig, err := io.ReadAll(block.ArmoredSignature.Body)
	if err != nil {
		return fmt.Errorf("read the inline signature: %w", err)
	}
	return a.checkDetached(block.Bytes, sig)
}

func (a *Audit) checkDetached(data, sig []byte) error {
	sigs, err := readSignaturePackets(sig)
	if err != nil {
		return err
	}
	var localErr, publishedErr error
	if a.local != nil {
		localErr = a.verify(openpgp.EntityList{a.local}, data, sig)
	}
	if a.published != nil {
		publishedErr = a.verify(a.published, data, sig)
	}
	issued := make([]Issued, 0, len(sigs))
	for _, s := range sigs {
		is := Issued{KeyID: signatureIssuer(s), LocalErr: localErr, PublishedErr: publishedErr}
		if h := weakHash(s.Hash); h != "" {
			is.Refused = "it uses the " + h + " digest, which current clients (RHEL 9 and later, apt since 2.x) refuse"
		}
		issued = append(issued, is)
	}
	return a.record(issued, false)
}

// Check records the signatures on one item the caller signed and inspected
// itself, such as an rpm. foreignOK allows signatures by other keys alongside
// the configured key's (a package can carry an earlier signature); without it
// any such signature is an error. With it, at least one signature must still
// be the configured key's.
func (a *Audit) Check(ctx context.Context, sigs []Issued, foreignOK bool) error {
	if a == nil {
		return nil
	}
	a.load(ctx)
	return a.record(sigs, foreignOK)
}

// verify checks sig over data against keys as a client would, including the
// keys' expiry and revocation.
func (a *Audit) verify(keys openpgp.EntityList, data, sig []byte) error {
	cfg := &packet.Config{Time: a.now}
	if _, err := openpgp.CheckArmoredDetachedSignature(keys, bytes.NewReader(data), bytes.NewReader(sig), cfg); err == nil {
		return nil
	}
	_, err := openpgp.CheckDetachedSignature(keys, bytes.NewReader(data), bytes.NewReader(sig), cfg)
	return err
}

// signerIDs are the long key ids that may sign for the configured key: its
// primary key's and its subkeys'. nil means the key is unknown.
func (a *Audit) signerIDs() map[string]bool {
	if a.local == nil {
		return nil
	}
	ids := map[string]bool{longID(a.local.PrimaryKey): true}
	for _, sk := range a.local.Subkeys {
		ids[longID(sk.PublicKey)] = true
	}
	return ids
}

func (a *Audit) record(sigs []Issued, foreignOK bool) error {
	allowed := a.signerIDs()
	var ours []Issued
	var foreign []string
	seenID := map[string]bool{}
	for _, s := range sigs {
		s.KeyID = strings.ToLower(s.KeyID)
		if allowed != nil && !allowed[s.KeyID] {
			foreign = append(foreign, s.KeyID)
			continue
		}
		// One item can carry several signatures by the same key (an rpm's
		// v4 and OPENPGP header signatures); it counts once.
		if seenID[s.KeyID] {
			continue
		}
		seenID[s.KeyID] = true
		ours = append(ours, s)
	}
	if len(sigs) == 0 {
		return fmt.Errorf("the signing produced no signature that could be read")
	}
	if allowed != nil && (len(ours) == 0 || (!foreignOK && len(foreign) > 0)) {
		return fmt.Errorf("signed by key %s, which is neither the configured key %s nor one of its subkeys; gpg chose a different key than the one specified (check that --gpg-key-id names exactly one key, or pass its fingerprint)",
			strings.Join(foreign, ", "), a.expected)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.items++
	if a.bySigner == nil {
		a.bySigner = map[string]int{}
		a.failures = map[string]*publishedFailure{}
	}
	for _, s := range ours {
		a.bySigner[s.KeyID]++
		if s.LocalErr != nil {
			a.noteLocked("a signature by %s does not verify even against your own copy of the key (%v); nobody will be able to verify it", s.KeyID, s.LocalErr)
		}
		if s.PublishedErr != nil && a.published != nil {
			f := a.failures[s.KeyID]
			if f == nil {
				f = &publishedFailure{reason: a.explainPublished(s.KeyID, s.PublishedErr)}
				a.failures[s.KeyID] = f
			}
			f.n++
		}
		if s.Refused != "" {
			a.noteLocked("a signature by %s may be refused: %s", s.KeyID, s.Refused)
		}
	}
	for _, id := range foreign {
		a.noteLocked("a signed package also carries a signature by %s, which is not the configured key %s; clients that do not trust %s ignore it", id, a.expected, id)
	}
	return nil
}

// explainPublished turns a failure to verify against the published key into
// the reason a person can act on.
func (a *Audit) explainPublished(issuer string, err error) string {
	var id uint64
	if _, scanErr := fmt.Sscanf(issuer, "%x", &id); scanErr == nil && len(a.published.KeysById(id)) == 0 {
		var held []string
		for _, e := range a.published {
			if FingerprintOf(e) == a.expected {
				return fmt.Sprintf("it holds key %s but not its subkey %s, which made them. Export the whole key, subkeys included, with gpg --armor --export %s and publish that", a.expected, issuer, a.expected)
			}
			held = append(held, FingerprintOf(e))
		}
		return fmt.Sprintf("it holds %s, not the signing key %s", strings.Join(held, ", "), a.expected)
	}
	reason := err.Error()
	if strings.Contains(reason, "expired") {
		reason += ". The published copy may predate a change to the key's expiry: export and publish the current public key"
	}
	return reason
}

func (a *Audit) note(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.noteLocked(format, args...)
}

func (a *Audit) noteLocked(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	if !a.seen[msg] {
		a.seen[msg] = true
		a.notes = append(a.notes, msg)
	}
}

// Warnings returns what Report prints as warnings, and the one-line summary
// of what was checked. Both are empty when nothing was signed.
func (a *Audit) Warnings() (warnings []string, summary string) {
	if a == nil {
		return nil, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.items == 0 {
		return nil, ""
	}

	signers := make([]string, 0, len(a.bySigner))
	for id := range a.bySigner {
		signers = append(signers, id)
	}
	sort.Strings(signers)

	primary := ""
	if a.local != nil {
		primary = longID(a.local.PrimaryKey)
	}
	for _, id := range signers {
		if primary == "" || id == primary {
			continue
		}
		w := fmt.Sprintf("%d of %d signed item(s) were signed by subkey %s of key %s, not by its primary key. gpg picks a key's signing subkey automatically, and a client verifies these signatures only if the public key it installs includes that subkey",
			a.bySigner[id], a.items, id, a.expected)
		switch {
		case a.published != nil && a.failures[id] == nil:
			w += fmt.Sprintf("; the public key at %s does", a.keyURL)
		case a.keyURL == "":
			w += fmt.Sprintf(". Publish the output of gpg --armor --export %s, which includes subkeys, and record its URL with --gpg-key-url so this is checked", a.expected)
		}
		warnings = append(warnings, w)
	}
	for _, id := range signers {
		if f := a.failures[id]; f != nil {
			warnings = append(warnings, fmt.Sprintf("the public key at %s does not verify %d signed item(s) made by %s, so your users cannot install from this repository: %s",
				a.keyURL, f.n, id, f.reason))
		}
	}
	warnings = append(warnings, a.expiryWarnings(signers)...)
	warnings = append(warnings, a.notes...)

	who := strings.Join(signers, ", ")
	switch {
	case a.published != nil && len(a.failures) == 0:
		summary = fmt.Sprintf("%d signed item(s) by %s, verified against the public key at %s", a.items, who, a.keyURL)
	case a.published != nil:
		summary = fmt.Sprintf("%d signed item(s) by %s; the public key at %s does NOT verify all of them", a.items, who, a.keyURL)
	case a.keyURL == "":
		summary = fmt.Sprintf("%d signed item(s) by %s; not checked against the key your users install, because no --gpg-key-url is recorded", a.items, who)
	default:
		summary = fmt.Sprintf("%d signed item(s) by %s; not checked against the public key at %s", a.items, who, a.keyURL)
	}
	return warnings, summary
}

// Report writes the warnings, then the summary, to w. It writes nothing when
// nothing was signed.
func (a *Audit) Report(w io.Writer) {
	warnings, summary := a.Warnings()
	for _, msg := range warnings {
		fmt.Fprintf(w, "warning: %s\n", msg)
	}
	if summary != "" {
		fmt.Fprintf(w, "signatures: %s\n", summary)
	}
}

// expiryWarnings flags each key that signed this run whose expiry, in the
// local copy or the published one, falls within the warning window, and a
// published copy that records an earlier expiry than the local one: clients
// go by the copy they installed, not by the signer's keyring.
func (a *Audit) expiryWarnings(signers []string) []string {
	if a.local == nil {
		return nil
	}
	window := a.ExpiryWarning
	if window == 0 {
		window = 30 * 24 * time.Hour
	}
	now := a.now()
	var out []string
	for _, id := range signers {
		local, ok := keyExpiry(a.local, id)
		if ok && local.Sub(now) < window {
			out = append(out, fmt.Sprintf("key %s expires on %s (%s), after which clients reject every signature it made; extend it (gpg --quick-set-expire) and publish the updated public key",
				id, local.Format("2006-01-02"), untilText(now, local)))
		}
		for _, e := range a.published {
			if FingerprintOf(e) != a.expected {
				continue
			}
			pub, pubOK := keyExpiry(e, id)
			switch {
			case !pubOK:
			case pub.Sub(now) < window:
				out = append(out, fmt.Sprintf("the public key at %s says key %s expires on %s (%s); clients go by that copy, so publish an updated one",
					a.keyURL, id, pub.Format("2006-01-02"), untilText(now, pub)))
			case !ok || pub.Before(local):
				out = append(out, fmt.Sprintf("the public key at %s says key %s expires on %s, earlier than your keyring does; clients go by the published copy, so publish the current public key before then",
					a.keyURL, id, pub.Format("2006-01-02")))
			}
		}
	}
	return out
}

func untilText(now, t time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "already expired"
	}
	return fmt.Sprintf("in %d day(s)", int(d.Hours()/24))
}

// keyExpiry reports when the key with long id id in e stops being usable: its
// own expiry or its primary key's, whichever is sooner. ok is false when
// neither expires or e does not hold the key.
func keyExpiry(e *openpgp.Entity, id string) (time.Time, bool) {
	var times []time.Time
	if sig, _ := e.PrimarySelfSignature(); sig != nil {
		if t, ok := expiryOf(e.PrimaryKey, sig); ok {
			times = append(times, t)
		}
	}
	found := longID(e.PrimaryKey) == id
	for _, sk := range e.Subkeys {
		if longID(sk.PublicKey) != id {
			continue
		}
		found = true
		if t, ok := expiryOf(sk.PublicKey, sk.Sig); ok {
			times = append(times, t)
		}
	}
	if !found || len(times) == 0 {
		return time.Time{}, false
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	return times[0], true
}

func expiryOf(pk *packet.PublicKey, sig *packet.Signature) (time.Time, bool) {
	if sig == nil || sig.KeyLifetimeSecs == nil || *sig.KeyLifetimeSecs == 0 {
		return time.Time{}, false
	}
	return pk.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second), true
}

// readSignaturePackets parses the signature packets in an armored or binary
// signature.
func readSignaturePackets(sig []byte) ([]*packet.Signature, error) {
	var r io.Reader = bytes.NewReader(sig)
	if block, err := armor.Decode(bytes.NewReader(sig)); err == nil {
		r = block.Body
	}
	var out []*packet.Signature
	pr := packet.NewReader(r)
	for {
		p, err := pr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the signature just made: %w", err)
		}
		if s, ok := p.(*packet.Signature); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the signing produced no signature packet")
	}
	return out, nil
}

// signatureIssuer is the long key id a signature names as its issuer.
func signatureIssuer(s *packet.Signature) string {
	if s.IssuerKeyId != nil {
		return fmt.Sprintf("%016x", *s.IssuerKeyId)
	}
	if n := len(s.IssuerFingerprint); n >= 8 {
		return fmt.Sprintf("%x", s.IssuerFingerprint[n-8:])
	}
	return "an unidentified key"
}

// weakHash names a digest clients refuse in signatures, or returns "".
func weakHash(h crypto.Hash) string {
	if h == crypto.MD4 || h == crypto.MD5 || h == crypto.SHA1 || h == crypto.RIPEMD160 {
		return h.String()
	}
	return ""
}

// longID is a key's long key id in the lowercase form signatureIssuer gives.
func longID(pk *packet.PublicKey) string { return fmt.Sprintf("%016x", pk.KeyId) }
