package mcp

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crosslink/internal/secret"
)

// Authenticator modifies an outbound HTTP request to carry authentication credentials.
type Authenticator interface {
	Apply(req *http.Request) error
}

type noAuth struct{}

func (a *noAuth) Apply(_ *http.Request) error { return nil }

type bearerAuth struct {
	token string
}

func (a *bearerAuth) Apply(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.token)
	return nil
}

type basicAuth struct {
	user string
	pass string
}

func (a *basicAuth) Apply(req *http.Request) error {
	cred := base64.StdEncoding.EncodeToString([]byte(a.user + ":" + a.pass))
	req.Header.Set("Authorization", "Basic "+cred)
	return nil
}

type oauth2Auth struct {
	clientID     string
	clientSecret string
	tokenURL     string
	scope        string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func (a *oauth2Auth) Apply(req *http.Request) error {
	token, err := a.getToken()
	if err != nil {
		return fmt.Errorf("oauth2 get token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (a *oauth2Auth) getToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.expiresAt) {
		return a.accessToken, nil
	}

	form := url.Values{"grant_type": {"client_credentials"}}
	if a.scope != "" {
		form.Set("scope", a.scope)
	}

	req, err := http.NewRequest(http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cred := base64.StdEncoding.EncodeToString([]byte(a.clientID + ":" + a.clientSecret))
	req.Header.Set("Authorization", "Basic "+cred)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token")
	}

	a.accessToken = tr.AccessToken
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl < 120*time.Second {
		ttl = 120 * time.Second
	}
	a.expiresAt = time.Now().Add(ttl - 60*time.Second)
	return a.accessToken, nil
}

type sigv4Auth struct {
	accessKeyID     string
	secretAccessKey string
	region          string
	service         string
}

func (a *sigv4Auth) Apply(req *http.Request) error {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	req.Header.Set("Host", host)

	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("read body for signing: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	payloadHash := sha256Hex(bodyBytes)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signedHeaders := a.getSignedHeaderNames(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.Query().Encode(),
		a.canonicalHeaders(req, signedHeaders),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, a.region, a.service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := hmacChain(
		[]byte("AWS4"+a.secretAccessKey),
		dateStamp, a.region, a.service, "aws4_request",
	)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		a.accessKeyID, credentialScope, signedHeaders, signature,
	))
	return nil
}

func (a *sigv4Auth) getSignedHeaderNames(req *http.Request) string {
	headers := make([]string, 0, len(req.Header))
	for k := range req.Header {
		headers = append(headers, strings.ToLower(k))
	}
	sort.Strings(headers)
	return strings.Join(headers, ";")
}

func (a *sigv4Auth) canonicalHeaders(req *http.Request, signedHeaders string) string {
	names := strings.Split(signedHeaders, ";")
	var b strings.Builder
	for _, name := range names {
		vals := req.Header.Values(name)
		if len(vals) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s:%s\n", name, strings.Join(vals, ","))
	}
	return b.String()
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hmacChain(key []byte, msgs ...string) []byte {
	for _, msg := range msgs {
		key = hmacSHA256(key, []byte(msg))
	}
	return key
}

// NewAuthenticator creates the appropriate authenticator based on authType and config.
func NewAuthenticator(authType string, raw json.RawMessage, decrypt func(string) string, transport *http.Transport) (Authenticator, error) {
	if authType == "" || authType == "none" || len(raw) == 0 {
		return &noAuth{}, nil
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse auth config: %w", err)
	}

	getStr := func(key string) string {
		v, _ := cfg[key].(string)
		if secret.IsSensitiveField(key) && v != "" {
			v = decrypt(v)
		}
		return v
	}

	switch authType {
	case "bearer":
		token := getStr("bearer_token")
		if token == "" {
			return &noAuth{}, nil
		}
		return &bearerAuth{token: token}, nil

	case "basic":
		user := getStr("username")
		pass := getStr("password")
		if user == "" {
			return nil, fmt.Errorf("basic auth: missing username")
		}
		return &basicAuth{user: user, pass: pass}, nil

	case "oauth2":
		clientID := getStr("client_id")
		clientSecret := getStr("client_secret")
		tokenURL := getStr("token_url")
		if clientID == "" || clientSecret == "" || tokenURL == "" {
			return nil, fmt.Errorf("oauth2: missing client_id, client_secret, or token_url")
		}
		if err := validateServerURL(tokenURL); err != nil {
			return nil, fmt.Errorf("oauth2: invalid token_url: %w", err)
		}
		t := transport
		if t == nil {
			t = newFallbackMCPTransport()
		}
		return &oauth2Auth{
			clientID:     clientID,
			clientSecret: clientSecret,
			tokenURL:     tokenURL,
			scope:        getStr("scope"),
			httpClient:   mcpHTTPClient(t, 5*time.Second),
		}, nil

	case "sigv4":
		accessKeyID := getStr("access_key_id")
		secretAccessKey := getStr("secret_access_key")
		region := getStr("region")
		service := getStr("service")
		if accessKeyID == "" || secretAccessKey == "" || region == "" || service == "" {
			return nil, fmt.Errorf("sigv4: missing access_key_id, secret_access_key, region, or service")
		}
		return &sigv4Auth{
			accessKeyID:     accessKeyID,
			secretAccessKey: secretAccessKey,
			region:          region,
			service:         service,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported auth type: %s", authType)
	}
}
