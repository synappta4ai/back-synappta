// Package storage signs direct-to-bucket uploads for S3-compatible object
// stores (MinIO). The API never proxies the bytes: it hands the browser a
// short-lived presigned PUT URL and the browser talks to the bucket directly.
//
// The signer is implemented here instead of pulling an SDK: AWS Signature V4
// query authentication is a few HMAC steps, and both the MinIO deployment and
// the tests only need PUT presigning.
package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// Algorithm is the only SigV4 variant we issue.
	Algorithm = "AWS4-HMAC-SHA256"
	// service is the SigV4 service name for object storage.
	service = "s3"
	// unsignedPayload is the constant used by presigned URLs: the body is not
	// known when the URL is signed.
	unsignedPayload = "UNSIGNED-PAYLOAD"
	// maxPresignTTL is S3's hard limit (7 days, the signing key lifetime).
	maxPresignTTL = 7 * 24 * time.Hour
)

// DefaultTTL is how long a signed URL stays valid when Config.TTL is empty.
const DefaultTTL = 15 * time.Minute

// Config is one bucket's connection data. It comes from the tenant's own
// storage configuration, never from the process environment.
type Config struct {
	Endpoint   string
	Bucket     string
	Region     string
	AccessKey  string
	SecretKey  string
	PublicBase string
	TTL        time.Duration
}

// Presigner signs upload URLs for one bucket on one endpoint.
type Presigner struct {
	endpoint   *url.URL // scheme://host[:port][/prefix]
	bucket     string
	region     string
	accessKey  string
	secretKey  string
	publicBase string // optional public/CDN base for read URLs
	ttl        time.Duration
}

// New validates the bucket config and builds a presigner for it. Callers only
// call it with a tenant that configured its own bucket; there is no global
// fallback, so no bucket means local disk storage.
func New(cfg Config) (*Presigner, error) {
	ep, err := ParseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	bucket := strings.TrimSpace(cfg.Bucket)
	if bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, fmt.Errorf("access key and secret key are required")
	}

	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	ttl = min(ttl, maxPresignTTL)

	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}

	return &Presigner{
		endpoint:   ep,
		bucket:     bucket,
		region:     region,
		accessKey:  strings.TrimSpace(cfg.AccessKey),
		secretKey:  strings.TrimSpace(cfg.SecretKey),
		publicBase: strings.TrimSpace(cfg.PublicBase),
		ttl:        ttl,
	}, nil
}

// ParseEndpoint validates an S3/MinIO endpoint and strips the trailing slash.
func ParseEndpoint(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	ep, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}
	if ep.Scheme != "http" && ep.Scheme != "https" {
		return nil, fmt.Errorf("invalid endpoint %q: scheme must be http or https", raw)
	}
	if ep.Host == "" {
		return nil, fmt.Errorf("invalid endpoint %q: missing host", raw)
	}
	if ep.RawQuery != "" || ep.Fragment != "" {
		return nil, fmt.Errorf("invalid endpoint %q: query strings and fragments are not supported", raw)
	}
	ep.Path = strings.TrimSuffix(ep.Path, "/")
	return ep, nil
}

// Bucket is the bucket uploads land in.
func (p *Presigner) Bucket() string { return p.bucket }

// Host is the endpoint hostname (no port), used to validate the endpoint
// before the server connects to it.
func (p *Presigner) Host() string { return p.endpoint.Hostname() }

// TTL is how long a signed URL stays valid.
func (p *Presigner) TTL() time.Duration { return p.ttl }

// PresignPut returns a presigned PUT URL for key. When contentType is not
// empty it is signed too, so the upload must send that exact Content-Type
// (returned in the "headers" map of the API response).
func (p *Presigner) PresignPut(key, contentType string, now time.Time) string {
	headers := map[string]string{"host": p.endpoint.Host}
	if contentType != "" {
		headers["content-type"] = contentType
	}
	return p.presign("PUT", p.canonicalURI(key), headers, now, p.ttl)
}

// HeadBucketURL signs a HEAD for the bucket itself: a cheap way to check the
// credentials and the bucket exist before saving a tenant configuration.
func (p *Presigner) HeadBucketURL(now time.Time) string {
	return p.presign("HEAD", "/"+uriEncode(p.bucket, true), map[string]string{"host": p.endpoint.Host}, now, p.ttl)
}

// PresignGet signs a GET for key: a short-lived read URL (browsers and the
// server itself use it, so range requests stay with the bucket).
func (p *Presigner) PresignGet(key string, now time.Time) string {
	return p.presign("GET", p.canonicalURI(key), map[string]string{"host": p.endpoint.Host}, now, p.ttl)
}

// PresignHead signs a HEAD for key, used to check an object's existence.
func (p *Presigner) PresignHead(key string, now time.Time) string {
	return p.presign("HEAD", p.canonicalURI(key), map[string]string{"host": p.endpoint.Host}, now, p.ttl)
}

// PresignDelete signs a DELETE for key, so an object can be cleaned up with
// the same narrow credentials used to upload it.
func (p *Presigner) PresignDelete(key string, now time.Time) string {
	return p.presign("DELETE", p.canonicalURI(key), map[string]string{"host": p.endpoint.Host}, now, p.ttl)
}

// PublicURL is the read URL for an uploaded key: the configured public base
// when set, otherwise the path-style bucket URL.
func (p *Presigner) PublicURL(key string) string {
	if p.publicBase != "" {
		return p.publicBase + "/" + uriEncode(key, false)
	}
	return p.endpoint.Scheme + "://" + p.endpoint.Host + p.canonicalURI(key)
}

// canonicalURI is the path-style URI: [/prefix]/<bucket>/<key>, encoded per
// SigV4 (every byte outside A-Za-z0-9-._~ except the '/' separators).
func (p *Presigner) canonicalURI(key string) string {
	prefix := strings.TrimSuffix(p.endpoint.Path, "/")
	return prefix + "/" + uriEncode(p.bucket, false) + "/" + uriEncode(key, false)
}

// presign builds the full URL: canonical request → string to sign → signature.
func (p *Presigner) presign(method, canonicalURI string, headers map[string]string, now time.Time, ttl time.Duration) string {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	scope := dateStamp + "/" + p.region + "/" + service + "/aws4_request"

	signedNames, canonicalHeaders := canonicalizeHeaders(headers)
	params := map[string]string{
		"X-Amz-Algorithm":     Algorithm,
		"X-Amz-Credential":    p.accessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(math.Round(ttl.Seconds()))),
		"X-Amz-SignedHeaders": signedNames,
	}
	query := canonicalQuery(params)

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		query,
		canonicalHeaders,
		signedNames,
		unsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		Algorithm,
		amzDate,
		scope,
		sha256Hex(canonicalRequest),
	}, "\n")

	signature := hmacHex(p.signingKey(dateStamp), stringToSign)

	host := p.endpoint.Scheme + "://" + p.endpoint.Host
	return host + canonicalURI + "?" + query + "&X-Amz-Signature=" + signature
}

// signingKey derives the SigV4 key: HMAC chain over date, region, service.
func (p *Presigner) signingKey(dateStamp string) []byte {
	k := hmacSHA256([]byte("AWS4"+p.secretKey), dateStamp)
	k = hmacSHA256(k, p.region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// canonicalizeHeaders lowercases, sorts and trims the signed headers and
// returns both the SignedHeaders list and the canonical headers block.
func canonicalizeHeaders(headers map[string]string) (signed, block string) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteString(":")
		b.WriteString(strings.TrimSpace(headers[name]))
		b.WriteString("\n")
	}
	return strings.Join(names, ";"), b.String()
}

// canonicalQuery encodes the parameters (sorted by name) per SigV4.
func canonicalQuery(params map[string]string) string {
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, uriEncode(name, true)+"="+uriEncode(params[name], true))
	}
	return strings.Join(pairs, "&")
}

// uriEncode implements AWS's UriEncode: unreserved characters stay literal
// (A-Z a-z 0-9 - . _ ~), everything else is percent-encoded with uppercase
// hex. Slashes stay literal only inside object keys (encodeSlash=false); in
// query values they are encoded. net/url's encoders do not follow AWS's rules
// (they leave '$&+,:;=@' alone), hence this helper.
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hmacHex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}
