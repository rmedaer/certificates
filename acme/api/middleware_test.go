package api

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/smallstep/assert"
	"github.com/smallstep/certificates/acme"
	"github.com/smallstep/certificates/authority/provisioner"
	"github.com/smallstep/cli/jose"
	"github.com/smallstep/nosql/database"
)

var testBody = []byte("foo")

func testNext(w http.ResponseWriter, r *http.Request) {
	w.Write(testBody)
}

func TestHandlerAddNonce(t *testing.T) {
	url := "https://ca.smallstep.com/acme/new-nonce"
	type test struct {
		auth       acme.Interface
		problem    *acme.Error
		statusCode int
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/AddNonce-error": func(t *testing.T) test {
			return test{
				auth: &mockAcmeAuthority{
					newNonce: func() (string, error) {
						return "", acme.ServerInternalErr(errors.New("force"))
					},
				},
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("force")),
			}
		},
		"ok": func(t *testing.T) test {
			return test{
				auth: &mockAcmeAuthority{
					newNonce: func() (string, error) {
						return "bar", nil
					},
				},
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(tc.auth).(*Handler)
			req := httptest.NewRequest("GET", url, nil)
			w := httptest.NewRecorder()
			h.addNonce(testNext)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, res.Header["Replay-Nonce"], []string{"bar"})
				assert.Equals(t, res.Header["Cache-Control"], []string{"no-store"})
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

func TestHandlerAddDirLink(t *testing.T) {
	url := "https://ca.smallstep.com/acme/new-nonce"
	prov := newProv()
	type test struct {
		auth       acme.Interface
		link       string
		statusCode int
		ctx        context.Context
		problem    *acme.Error
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-provisioner": func(t *testing.T) test {
			return test{
				auth:       &mockAcmeAuthority{},
				ctx:        context.Background(),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"fail/nil-provisioner": func(t *testing.T) test {
			return test{
				auth:       &mockAcmeAuthority{},
				ctx:        context.WithValue(context.Background(), provisionerContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"ok": func(t *testing.T) test {
			link := "https://ca.smallstep.com/acme/directory"
			return test{
				auth: &mockAcmeAuthority{
					getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
						assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
						return link
					},
				},
				ctx:        context.WithValue(context.Background(), provisionerContextKey, prov),
				link:       link,
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(tc.auth).(*Handler)
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.addDirLink(testNext)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, res.Header["Link"], []string{fmt.Sprintf("<%s>;rel=\"index\"", tc.link)})
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

func TestHandlerVerifyContentType(t *testing.T) {
	prov := newProv()
	url := fmt.Sprintf("https://ca.smallstep.com/acme/%s/certificate/abc123",
		acme.URLSafeProvisionerName(prov))
	type test struct {
		h           Handler
		ctx         context.Context
		contentType string
		problem     *acme.Error
		statusCode  int
		url         string
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-provisioner": func(t *testing.T) test {
			return test{
				h:          Handler{Auth: &mockAcmeAuthority{}},
				ctx:        context.Background(),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"fail/nil-provisioner": func(t *testing.T) test {
			return test{
				h:          Handler{Auth: &mockAcmeAuthority{}},
				ctx:        context.WithValue(context.Background(), provisionerContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"fail/general-bad-content-type": func(t *testing.T) test {
			return test{
				h: Handler{
					Auth: &mockAcmeAuthority{
						getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
							assert.Equals(t, typ, acme.CertificateLink)
							assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
							assert.Equals(t, abs, false)
							assert.Equals(t, in, []string{""})
							return "/certificate/"
						},
					},
				},
				url: fmt.Sprintf("https://ca.smallstep.com/acme/%s/new-account",
					acme.URLSafeProvisionerName(prov)),
				ctx:         context.WithValue(context.Background(), provisionerContextKey, prov),
				contentType: "foo",
				statusCode:  400,
				problem:     acme.MalformedErr(errors.New("expected content-type to be in [application/jose+json], but got foo")),
			}
		},
		"fail/certificate-bad-content-type": func(t *testing.T) test {
			return test{
				h: Handler{
					Auth: &mockAcmeAuthority{
						getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
							assert.Equals(t, typ, acme.CertificateLink)
							assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
							assert.Equals(t, abs, false)
							assert.Equals(t, in, []string{""})
							return "/certificate/"
						},
					},
				},
				ctx:         context.WithValue(context.Background(), provisionerContextKey, prov),
				contentType: "foo",
				statusCode:  400,
				problem:     acme.MalformedErr(errors.New("expected content-type to be in [application/jose+json application/pkix-cert application/pkcs7-mime], but got foo")),
			}
		},
		"ok": func(t *testing.T) test {
			return test{
				h: Handler{
					Auth: &mockAcmeAuthority{
						getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
							assert.Equals(t, typ, acme.CertificateLink)
							assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
							assert.Equals(t, abs, false)
							assert.Equals(t, in, []string{""})
							return "/certificate/"
						},
					},
				},
				ctx:         context.WithValue(context.Background(), provisionerContextKey, prov),
				contentType: "application/jose+json",
				statusCode:  200,
			}
		},
		"ok/certificate/pkix-cert": func(t *testing.T) test {
			return test{
				h: Handler{
					Auth: &mockAcmeAuthority{
						getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
							assert.Equals(t, typ, acme.CertificateLink)
							assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
							assert.Equals(t, abs, false)
							assert.Equals(t, in, []string{""})
							return "/certificate/"
						},
					},
				},
				ctx:         context.WithValue(context.Background(), provisionerContextKey, prov),
				contentType: "application/pkix-cert",
				statusCode:  200,
			}
		},
		"ok/certificate/jose+json": func(t *testing.T) test {
			return test{
				h: Handler{
					Auth: &mockAcmeAuthority{
						getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
							assert.Equals(t, typ, acme.CertificateLink)
							assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
							assert.Equals(t, abs, false)
							assert.Equals(t, in, []string{""})
							return "/certificate/"
						},
					},
				},
				ctx:         context.WithValue(context.Background(), provisionerContextKey, prov),
				contentType: "application/jose+json",
				statusCode:  200,
			}
		},
		"ok/certificate/pkcs7-mime": func(t *testing.T) test {
			return test{
				h: Handler{
					Auth: &mockAcmeAuthority{
						getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
							assert.Equals(t, typ, acme.CertificateLink)
							assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
							assert.Equals(t, abs, false)
							assert.Equals(t, in, []string{""})
							return "/certificate/"
						},
					},
				},
				ctx:         context.WithValue(context.Background(), provisionerContextKey, prov),
				contentType: "application/pkcs7-mime",
				statusCode:  200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			_url := url
			if tc.url != "" {
				_url = tc.url
			}
			req := httptest.NewRequest("GET", _url, nil)
			req = req.WithContext(tc.ctx)
			req.Header.Add("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			tc.h.verifyContentType(testNext)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

func TestHandlerIsPostAsGet(t *testing.T) {
	url := "https://ca.smallstep.com/acme/new-account"
	type test struct {
		ctx        context.Context
		problem    *acme.Error
		statusCode int
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-payload": func(t *testing.T) test {
			return test{
				ctx:        context.Background(),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("payload expected in request context")),
			}
		},
		"fail/nil-payload": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), payloadContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("payload expected in request context")),
			}
		},
		"fail/not-post-as-get": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), payloadContextKey, &payloadInfo{}),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("expected POST-as-GET")),
			}
		},
		"ok": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), payloadContextKey, &payloadInfo{isPostAsGet: true}),
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(nil).(*Handler)
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.isPostAsGet(testNext)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

type errReader int

func (errReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("force")
}
func (errReader) Close() error {
	return nil
}

func TestHandlerParseJWS(t *testing.T) {
	url := "https://ca.smallstep.com/acme/new-account"
	type test struct {
		next       nextHTTP
		body       io.Reader
		problem    *acme.Error
		statusCode int
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/read-body-error": func(t *testing.T) test {
			return test{
				body:       errReader(0),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("failed to read request body: force")),
			}
		},
		"fail/parse-jws-error": func(t *testing.T) test {
			return test{
				body:       strings.NewReader("foo"),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("failed to parse JWS from request body: square/go-jose: compact JWS format must have three parts")),
			}
		},
		"ok": func(t *testing.T) test {
			jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			signer, err := jose.NewSigner(jose.SigningKey{
				Algorithm: jose.SignatureAlgorithm(jwk.Algorithm),
				Key:       jwk.Key,
			}, new(jose.SignerOptions))
			assert.FatalError(t, err)
			signed, err := signer.Sign([]byte("baz"))
			assert.FatalError(t, err)
			expRaw, err := signed.CompactSerialize()
			assert.FatalError(t, err)

			return test{
				body: strings.NewReader(expRaw),
				next: func(w http.ResponseWriter, r *http.Request) {
					jws, err := jwsFromContext(r)
					assert.FatalError(t, err)
					gotRaw, err := jws.CompactSerialize()
					assert.FatalError(t, err)
					assert.Equals(t, gotRaw, expRaw)
					w.Write(testBody)
				},
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(nil).(*Handler)
			req := httptest.NewRequest("GET", url, tc.body)
			w := httptest.NewRecorder()
			h.parseJWS(tc.next)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

func TestHandlerVerifyAndExtractJWSPayload(t *testing.T) {
	jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
	assert.FatalError(t, err)
	_pub := jwk.Public()
	pub := &_pub
	so := new(jose.SignerOptions)
	so.WithHeader("alg", jose.SignatureAlgorithm(jwk.Algorithm))
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.SignatureAlgorithm(jwk.Algorithm),
		Key:       jwk.Key,
	}, so)
	assert.FatalError(t, err)
	jws, err := signer.Sign([]byte("baz"))
	assert.FatalError(t, err)
	raw, err := jws.CompactSerialize()
	assert.FatalError(t, err)
	parsedJWS, err := jose.ParseJWS(raw)
	assert.FatalError(t, err)
	url := "https://ca.smallstep.com/acme/account/1234"
	type test struct {
		ctx        context.Context
		next       func(http.ResponseWriter, *http.Request)
		problem    *acme.Error
		statusCode int
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-jws": func(t *testing.T) test {
			return test{
				ctx:        context.Background(),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/nil-jws": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/no-jwk": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jwk expected in request context")),
			}
		},
		"fail/nil-jwk": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), jwsContextKey, parsedJWS)
			return test{
				ctx:        context.WithValue(ctx, jwkContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jwk expected in request context")),
			}
		},
		"fail/verify-jws-failure": func(t *testing.T) test {
			_jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			_pub := _jwk.Public()
			ctx := context.WithValue(context.Background(), jwsContextKey, parsedJWS)
			ctx = context.WithValue(ctx, jwkContextKey, &_pub)
			return test{
				ctx:        ctx,
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("error verifying jws: square/go-jose: error in cryptographic primitive")),
			}
		},
		"fail/algorithm-mismatch": func(t *testing.T) test {
			_pub := *pub
			clone := &_pub
			clone.Algorithm = jose.HS256
			ctx := context.WithValue(context.Background(), jwsContextKey, parsedJWS)
			ctx = context.WithValue(ctx, jwkContextKey, clone)
			return test{
				ctx:        ctx,
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("verifier and signature algorithm do not match")),
			}
		},
		"ok": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), jwsContextKey, parsedJWS)
			ctx = context.WithValue(ctx, jwkContextKey, pub)
			return test{
				ctx:        ctx,
				statusCode: 200,
				next: func(w http.ResponseWriter, r *http.Request) {
					p, err := payloadFromContext(r)
					assert.FatalError(t, err)
					if assert.NotNil(t, p) {
						assert.Equals(t, p.value, []byte("baz"))
						assert.False(t, p.isPostAsGet)
						assert.False(t, p.isEmptyJSON)
					}
					w.Write(testBody)
				},
			}
		},
		"ok/empty-algorithm-in-jwk": func(t *testing.T) test {
			_pub := *pub
			clone := &_pub
			clone.Algorithm = ""
			ctx := context.WithValue(context.Background(), jwsContextKey, parsedJWS)
			ctx = context.WithValue(ctx, jwkContextKey, pub)
			return test{
				ctx:        ctx,
				statusCode: 200,
				next: func(w http.ResponseWriter, r *http.Request) {
					p, err := payloadFromContext(r)
					assert.FatalError(t, err)
					if assert.NotNil(t, p) {
						assert.Equals(t, p.value, []byte("baz"))
						assert.False(t, p.isPostAsGet)
						assert.False(t, p.isEmptyJSON)
					}
					w.Write(testBody)
				},
			}
		},
		"ok/post-as-get": func(t *testing.T) test {
			_jws, err := signer.Sign([]byte(""))
			assert.FatalError(t, err)
			_raw, err := _jws.CompactSerialize()
			assert.FatalError(t, err)
			_parsed, err := jose.ParseJWS(_raw)
			assert.FatalError(t, err)
			ctx := context.WithValue(context.Background(), jwsContextKey, _parsed)
			ctx = context.WithValue(ctx, jwkContextKey, pub)
			return test{
				ctx:        ctx,
				statusCode: 200,
				next: func(w http.ResponseWriter, r *http.Request) {
					p, err := payloadFromContext(r)
					assert.FatalError(t, err)
					if assert.NotNil(t, p) {
						assert.Equals(t, p.value, []byte{})
						assert.True(t, p.isPostAsGet)
						assert.False(t, p.isEmptyJSON)
					}
					w.Write(testBody)
				},
			}
		},
		"ok/empty-json": func(t *testing.T) test {
			_jws, err := signer.Sign([]byte("{}"))
			assert.FatalError(t, err)
			_raw, err := _jws.CompactSerialize()
			assert.FatalError(t, err)
			_parsed, err := jose.ParseJWS(_raw)
			assert.FatalError(t, err)
			ctx := context.WithValue(context.Background(), jwsContextKey, _parsed)
			ctx = context.WithValue(ctx, jwkContextKey, pub)
			return test{
				ctx:        ctx,
				statusCode: 200,
				next: func(w http.ResponseWriter, r *http.Request) {
					p, err := payloadFromContext(r)
					assert.FatalError(t, err)
					if assert.NotNil(t, p) {
						assert.Equals(t, p.value, []byte("{}"))
						assert.False(t, p.isPostAsGet)
						assert.True(t, p.isEmptyJSON)
					}
					w.Write(testBody)
				},
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(nil).(*Handler)
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.verifyAndExtractJWSPayload(tc.next)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

func TestHandlerLookupJWK(t *testing.T) {
	prov := newProv()
	url := fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/1234",
		acme.URLSafeProvisionerName(prov))
	jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
	assert.FatalError(t, err)
	accID := "account-id"
	prefix := fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/",
		acme.URLSafeProvisionerName(prov))
	so := new(jose.SignerOptions)
	so.WithHeader("kid", fmt.Sprintf("%s%s", prefix, accID))
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.SignatureAlgorithm(jwk.Algorithm),
		Key:       jwk.Key,
	}, so)
	assert.FatalError(t, err)
	jws, err := signer.Sign([]byte("baz"))
	assert.FatalError(t, err)
	raw, err := jws.CompactSerialize()
	assert.FatalError(t, err)
	parsedJWS, err := jose.ParseJWS(raw)
	assert.FatalError(t, err)
	type test struct {
		auth       acme.Interface
		ctx        context.Context
		next       func(http.ResponseWriter, *http.Request)
		problem    *acme.Error
		statusCode int
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-provisioner": func(t *testing.T) test {
			return test{
				ctx:        context.Background(),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"fail/nil-provisioner": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), provisionerContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"fail/no-jws": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), provisionerContextKey, prov),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/nil-jws": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, nil)
			return test{
				ctx:        ctx,
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/no-kid": func(t *testing.T) test {
			_signer, err := jose.NewSigner(jose.SigningKey{
				Algorithm: jose.SignatureAlgorithm(jwk.Algorithm),
				Key:       jwk.Key,
			}, new(jose.SignerOptions))
			assert.FatalError(t, err)
			_jws, err := _signer.Sign([]byte("baz"))
			assert.FatalError(t, err)
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, _jws)
			return test{
				auth: &mockAcmeAuthority{
					getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
						assert.Equals(t, typ, acme.AccountLink)
						assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
						assert.True(t, abs)
						assert.Equals(t, in, []string{""})
						return prefix
					},
				},
				ctx:        ctx,
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("kid does not have required prefix; expected %s, but got ", prefix)),
			}
		},
		"fail/bad-kid-prefix": func(t *testing.T) test {
			_so := new(jose.SignerOptions)
			_so.WithHeader("kid", "foo")
			_signer, err := jose.NewSigner(jose.SigningKey{
				Algorithm: jose.SignatureAlgorithm(jwk.Algorithm),
				Key:       jwk.Key,
			}, _so)
			assert.FatalError(t, err)
			_jws, err := _signer.Sign([]byte("baz"))
			assert.FatalError(t, err)
			_raw, err := _jws.CompactSerialize()
			assert.FatalError(t, err)
			_parsed, err := jose.ParseJWS(_raw)
			assert.FatalError(t, err)
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, _parsed)
			return test{
				auth: &mockAcmeAuthority{
					getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
						assert.Equals(t, typ, acme.AccountLink)
						assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
						assert.True(t, abs)
						assert.Equals(t, in, []string{""})
						return fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/", acme.URLSafeProvisionerName(prov))
					},
				},
				ctx:        ctx,
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("kid does not have required prefix; expected %s, but got foo", prefix)),
			}
		},
		"fail/account-not-found": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				auth: &mockAcmeAuthority{
					getAccount: func(p provisioner.Interface, _accID string) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, accID, accID)
						return nil, database.ErrNotFound
					},
					getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
						assert.Equals(t, typ, acme.AccountLink)
						assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
						assert.True(t, abs)
						assert.Equals(t, in, []string{""})
						return fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/", acme.URLSafeProvisionerName(prov))
					},
				},
				ctx:        ctx,
				statusCode: 404,
				problem:    acme.AccountDoesNotExistErr(nil),
			}
		},
		"fail/GetAccount-error": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				auth: &mockAcmeAuthority{
					getAccount: func(p provisioner.Interface, _accID string) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, accID, accID)
						return nil, acme.ServerInternalErr(errors.New("force"))
					},
					getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
						assert.Equals(t, typ, acme.AccountLink)
						assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
						assert.True(t, abs)
						assert.Equals(t, in, []string{""})
						return fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/", acme.URLSafeProvisionerName(prov))
					},
				},
				ctx:        ctx,
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("force")),
			}
		},
		"fail/account-not-valid": func(t *testing.T) test {
			acc := &acme.Account{Status: "deactivated"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				auth: &mockAcmeAuthority{
					getAccount: func(p provisioner.Interface, _accID string) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, accID, accID)
						return acc, nil
					},
					getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
						assert.Equals(t, typ, acme.AccountLink)
						assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
						assert.True(t, abs)
						assert.Equals(t, in, []string{""})
						return fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/", acme.URLSafeProvisionerName(prov))
					},
				},
				ctx:        ctx,
				statusCode: 401,
				problem:    acme.UnauthorizedErr(errors.New("account is not active")),
			}
		},
		"ok": func(t *testing.T) test {
			acc := &acme.Account{Status: "valid", Key: jwk}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				auth: &mockAcmeAuthority{
					getAccount: func(p provisioner.Interface, _accID string) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, accID, accID)
						return acc, nil
					},
					getLink: func(typ acme.Link, provID string, abs bool, in ...string) string {
						assert.Equals(t, typ, acme.AccountLink)
						assert.Equals(t, provID, acme.URLSafeProvisionerName(prov))
						assert.True(t, abs)
						assert.Equals(t, in, []string{""})
						return fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/", acme.URLSafeProvisionerName(prov))
					},
				},
				ctx: ctx,
				next: func(w http.ResponseWriter, r *http.Request) {
					_acc, err := accountFromContext(r)
					assert.FatalError(t, err)
					assert.Equals(t, _acc, acc)
					_jwk, err := jwkFromContext(r)
					assert.FatalError(t, err)
					assert.Equals(t, _jwk, jwk)
					w.Write(testBody)
				},
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(tc.auth).(*Handler)
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.lookupJWK(tc.next)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

func TestHandlerExtractJWK(t *testing.T) {
	prov := newProv()
	jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
	assert.FatalError(t, err)
	kid, err := jwk.Thumbprint(crypto.SHA256)
	assert.FatalError(t, err)
	pub := jwk.Public()
	pub.KeyID = base64.RawURLEncoding.EncodeToString(kid)

	so := new(jose.SignerOptions)
	so.WithHeader("jwk", pub)
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.SignatureAlgorithm(jwk.Algorithm),
		Key:       jwk.Key,
	}, so)
	assert.FatalError(t, err)
	jws, err := signer.Sign([]byte("baz"))
	assert.FatalError(t, err)
	raw, err := jws.CompactSerialize()
	assert.FatalError(t, err)
	parsedJWS, err := jose.ParseJWS(raw)
	assert.FatalError(t, err)
	url := fmt.Sprintf("https://ca.smallstep.com/acme/%s/account/1234",
		acme.URLSafeProvisionerName(prov))
	type test struct {
		auth       acme.Interface
		ctx        context.Context
		next       func(http.ResponseWriter, *http.Request)
		problem    *acme.Error
		statusCode int
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-provisioner": func(t *testing.T) test {
			return test{
				ctx:        context.Background(),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"fail/nil-provisioner": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), provisionerContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("provisioner expected in request context")),
			}
		},
		"fail/no-jws": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), provisionerContextKey, prov),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/nil-jws": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, nil)
			return test{
				ctx:        ctx,
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/nil-jwk": func(t *testing.T) test {
			_jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							JSONWebKey: nil,
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, _jws)
			return test{
				ctx:        ctx,
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("jwk expected in protected header")),
			}
		},
		"fail/invalid-jwk": func(t *testing.T) test {
			_jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							JSONWebKey: &jose.JSONWebKey{Key: "foo"},
						},
					},
				},
			}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, _jws)
			return test{
				ctx:        ctx,
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("invalid jwk in protected header")),
			}
		},
		"fail/GetAccountByKey-error": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				ctx: ctx,
				auth: &mockAcmeAuthority{
					getAccountByKey: func(p provisioner.Interface, jwk *jose.JSONWebKey) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, jwk.KeyID, pub.KeyID)
						return nil, acme.ServerInternalErr(errors.New("force"))
					},
				},
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("force")),
			}
		},
		"fail/account-not-valid": func(t *testing.T) test {
			acc := &acme.Account{Status: "deactivated"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				ctx: ctx,
				auth: &mockAcmeAuthority{
					getAccountByKey: func(p provisioner.Interface, jwk *jose.JSONWebKey) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, jwk.KeyID, pub.KeyID)
						return acc, nil
					},
				},
				statusCode: 401,
				problem:    acme.UnauthorizedErr(errors.New("account is not active")),
			}
		},
		"ok": func(t *testing.T) test {
			acc := &acme.Account{Status: "valid"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				ctx: ctx,
				auth: &mockAcmeAuthority{
					getAccountByKey: func(p provisioner.Interface, jwk *jose.JSONWebKey) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, jwk.KeyID, pub.KeyID)
						return acc, nil
					},
				},
				next: func(w http.ResponseWriter, r *http.Request) {
					_acc, err := accountFromContext(r)
					assert.FatalError(t, err)
					assert.Equals(t, _acc, acc)
					_jwk, err := jwkFromContext(r)
					assert.FatalError(t, err)
					assert.Equals(t, _jwk.KeyID, pub.KeyID)
					w.Write(testBody)
				},
				statusCode: 200,
			}
		},
		"ok/no-account": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, jwsContextKey, parsedJWS)
			return test{
				ctx: ctx,
				auth: &mockAcmeAuthority{
					getAccountByKey: func(p provisioner.Interface, jwk *jose.JSONWebKey) (*acme.Account, error) {
						assert.Equals(t, p, prov)
						assert.Equals(t, jwk.KeyID, pub.KeyID)
						return nil, database.ErrNotFound
					},
				},
				next: func(w http.ResponseWriter, r *http.Request) {
					_acc, err := accountFromContext(r)
					assert.NotNil(t, err)
					assert.Nil(t, _acc)
					_jwk, err := jwkFromContext(r)
					assert.FatalError(t, err)
					assert.Equals(t, _jwk.KeyID, pub.KeyID)
					w.Write(testBody)
				},
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(tc.auth).(*Handler)
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.extractJWK(tc.next)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}

func TestHandlerValidateJWS(t *testing.T) {
	url := "https://ca.smallstep.com/acme/account/1234"
	type test struct {
		auth       acme.Interface
		ctx        context.Context
		next       func(http.ResponseWriter, *http.Request)
		problem    *acme.Error
		statusCode int
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-jws": func(t *testing.T) test {
			return test{
				ctx:        context.Background(),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/nil-jws": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, nil),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("jws expected in request context")),
			}
		},
		"fail/no-signature": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, &jose.JSONWebSignature{}),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("request body does not contain a signature")),
			}
		},
		"fail/more-than-one-signature": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{},
					{},
				},
			}
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("request body contains more than one signature")),
			}
		},
		"fail/unprotected-header-not-empty": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{Unprotected: jose.Header{Nonce: "abc"}},
				},
			}
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("unprotected header must not be used")),
			}
		},
		"fail/unsuitable-algorithm-none": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{Protected: jose.Header{Algorithm: "none"}},
				},
			}
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("unsuitable algorithm: none")),
			}
		},
		"fail/unsuitable-algorithm-mac": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{Protected: jose.Header{Algorithm: jose.HS256}},
				},
			}
			return test{
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("unsuitable algorithm: %s", jose.HS256)),
			}
		},
		"fail/rsa-key-&-alg-mismatch": func(t *testing.T) test {
			jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			pub := jwk.Public()
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm:  jose.RS256,
							JSONWebKey: &pub,
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": url,
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("jws key type and algorithm do not match")),
			}
		},
		"fail/rsa-key-too-small": func(t *testing.T) test {
			jwk, err := jose.GenerateJWK("RSA", "", "", "sig", "", 1024)
			assert.FatalError(t, err)
			pub := jwk.Public()
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm:  jose.RS256,
							JSONWebKey: &pub,
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": url,
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("rsa keys must be at least 2048 bits (256 bytes) in size")),
			}
		},
		"fail/UseNonce-error": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{Protected: jose.Header{Algorithm: jose.ES256}},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return acme.ServerInternalErr(errors.New("force"))
					},
				},
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 500,
				problem:    acme.ServerInternalErr(errors.New("force")),
			}
		},
		"fail/no-url-header": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{Protected: jose.Header{Algorithm: jose.ES256}},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.New("jws missing url protected header")),
			}
		},
		"fail/url-mismatch": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": "foo",
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("url header in JWS (foo) does not match request url (%s)", url)),
			}
		},
		"fail/both-jwk-kid": func(t *testing.T) test {
			jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			pub := jwk.Public()
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm:  jose.ES256,
							KeyID:      "bar",
							JSONWebKey: &pub,
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": url,
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("jwk and kid are mutually exclusive")),
			}
		},
		"fail/no-jwk-kid": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": url,
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx:        context.WithValue(context.Background(), jwsContextKey, jws),
				statusCode: 400,
				problem:    acme.MalformedErr(errors.Errorf("either jwk or kid must be defined in jws protected header")),
			}
		},
		"ok/kid": func(t *testing.T) test {
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm: jose.ES256,
							KeyID:     "bar",
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": url,
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx: context.WithValue(context.Background(), jwsContextKey, jws),
				next: func(w http.ResponseWriter, r *http.Request) {
					w.Write(testBody)
				},
				statusCode: 200,
			}
		},
		"ok/jwk/ecdsa": func(t *testing.T) test {
			jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			pub := jwk.Public()
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm:  jose.ES256,
							JSONWebKey: &pub,
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": url,
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx: context.WithValue(context.Background(), jwsContextKey, jws),
				next: func(w http.ResponseWriter, r *http.Request) {
					w.Write(testBody)
				},
				statusCode: 200,
			}
		},
		"ok/jwk/rsa": func(t *testing.T) test {
			jwk, err := jose.GenerateJWK("RSA", "", "", "sig", "", 2048)
			assert.FatalError(t, err)
			pub := jwk.Public()
			jws := &jose.JSONWebSignature{
				Signatures: []jose.Signature{
					{
						Protected: jose.Header{
							Algorithm:  jose.RS256,
							JSONWebKey: &pub,
							ExtraHeaders: map[jose.HeaderKey]interface{}{
								"url": url,
							},
						},
					},
				},
			}
			return test{
				auth: &mockAcmeAuthority{
					useNonce: func(n string) error {
						return nil
					},
				},
				ctx: context.WithValue(context.Background(), jwsContextKey, jws),
				next: func(w http.ResponseWriter, r *http.Request) {
					w.Write(testBody)
				},
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := New(tc.auth).(*Handler)
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.validateJWS(tc.next)(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.problem) {
				var ae acme.AError
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))
				prob := tc.problem.ToACME()

				assert.Equals(t, ae.Type, prob.Type)
				assert.Equals(t, ae.Detail, prob.Detail)
				assert.Equals(t, ae.Identifier, prob.Identifier)
				assert.Equals(t, ae.Subproblems, prob.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), testBody)
			}
		})
	}
}
