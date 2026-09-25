//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 只替换目的地址，仍使用真实 TCP 和 net/http 序列化。断言读取接收端的数据，
// 不读取构造器或 mock recorder；任何请求均无法访问公网。
type localCodexWireTransport struct{ target string }

func (u localCodexWireTransport) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target, err := url.Parse(u.target)
	if err != nil {
		return nil, err
	}
	clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	return client.Do(clone)
}

func (u localCodexWireTransport) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

type localCodexWireRequest struct {
	headers http.Header
	body    []byte
	err     error
}

func TestCodexLocalWireHTTP(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%v/passthrough=%v", enabled, passthrough), func(t *testing.T) {
				seen := make(chan localCodexWireRequest, 8)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					seen <- localCodexWireRequest{r.Header.Clone(), body, err}
					w.Header().Add("Set-Cookie", "__oailb=local-route; Path=/; Secure; HttpOnly")
					w.Header().Add("Set-Cookie", "session=must-not-replay; Path=/; Secure")
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, rawRelaySSE)
				}))
				defer server.Close()
				svc, _ := wireProfileTestService()
				svc.httpUpstream = localCodexWireTransport{server.URL}
				account := wireProfileTestAccount(enabled)
				account.Extra["openai_passthrough"] = passthrough
				account.Extra["openai_turn_state_override"] = map[string]any{"gpt-5.5": turnStateBlob(openAIHealthyTurnStateLen)}
				seed := account.Extra[codexFingerprintSeedExtraKey]
				for attempt := 0; attempt < 3; attempt++ {
					account.Extra["openai_turn_state_auto"] = attempt > 0
					if attempt == 2 {
						account.Credentials["chatgpt_account_id"] = "other-credential"
					}
					body := wireProfileTestBody(t)
					meta, err := sjson.Set(convTestTurnMetadata(), "model", "stale-model")
					require.NoError(t, err)
					body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", meta)
					require.NoError(t, err)
					c := newConvTestContext(t, body)
					c.Request.Header.Set(openAIWSTurnMetadataHeader, meta)
					_, err = svc.Forward(context.Background(), c, account, body)
					require.NoError(t, err)
					got := rawRelayRecv(t, seen)
					require.NoError(t, got.err)
					require.NotContains(t, got.headers.Get("User-Agent"), "Go-http-client")
					require.Empty(t, got.headers.Get(openAICodexTurnStateHeader), "禁止恢复手填或自动注入")
					require.Equal(t, attempt == 1, strings.Contains(got.headers.Get("Cookie"), "__oailb=local-route"))
					require.NotContains(t, got.headers.Get("Cookie"), "session=")
					if enabled {
						require.Equal(t, "zstd", got.headers.Get("Content-Encoding"))
						require.Greater(t, len(got.body), 6)
						require.Equal(t, []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x58}, got.body[:6])
						decoder, err := zstd.NewReader(nil)
						require.NoError(t, err)
						got.body, err = decoder.DecodeAll(got.body, nil)
						decoder.Close()
						require.NoError(t, err)
						outModel := gjson.GetBytes(got.body, "model").String()
						require.Equal(t, outModel, gjson.Get(got.headers.Get(openAIWSTurnMetadataHeader), "model").String())
						require.Equal(t, outModel, gjson.Get(gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String(), "model").String())
						require.Equal(t, got.headers.Get("session-id"), gjson.GetBytes(got.body, "prompt_cache_key").String())
						require.NotEmpty(t, got.headers.Get("thread-id"))
						require.Equal(t, got.headers.Get("thread-id"), got.headers.Get("x-client-request-id"))
					} else {
						require.Empty(t, got.headers.Get("Content-Encoding"))
					}
					requireCodexGuardianCreditsRequested(t, got.body, enabled)
					require.Equal(t, seed, account.Extra[codexFingerprintSeedExtraKey])
				}
				t.Log("receiver verified 3 real HTTP requests: headers, body, compression, cookies, credential rotation, no turn-state injection")
			})
		}
	}
}

// 真实生产 WS dialer，只把拨号地址固定到本地接收端；不替换连接或帧发送实现。
type localCodexWireDialer struct{ target string }

func (d localCodexWireDialer) Dial(ctx context.Context, _ string, h http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	return newDefaultOpenAIWSClientDialer().Dial(ctx, d.target, h, "")
}

func TestCodexLocalWireWebSocket(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%v", mode, enabled), func(t *testing.T) {
				headers := make(chan http.Header, 4)
				frames := make(chan []byte, 8)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headers <- r.Header.Clone()
					w.Header().Add("Set-Cookie", "__oailb=ws-route; Path=/; Secure")
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer conn.CloseNow()
					for turn := 1; ; turn++ {
						ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
						_, payload, err := conn.Read(ctx)
						cancel()
						if err != nil {
							return
						}
						frames <- payload
						if err := conn.Write(r.Context(), coderws.MessageText, codexWSCompletedEvent(fmt.Sprintf("resp_local_%d", turn))); err != nil {
							return
						}
					}
				}))
				defer server.Close()
				cfg := codexWSWireProfileConfig()
				svc := codexWSWireProfileService(cfg)
				account := wireProfileTestAccount(enabled)
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
				manual := turnStateBlob(openAIHealthyTurnStateLen)
				account.Extra["openai_turn_state_override"] = map[string]any{"gpt-5.5": manual}
				svc.accountRepo = &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
				dialer := localCodexWireDialer{"ws" + strings.TrimPrefix(server.URL, "http")}
				svc.openaiWSPassthroughDialer = dialer
				pool := svc.getOpenAIWSConnPool()
				pool.setClientDialerForTest(dialer)
				defer pool.Close()
				logicalURL, err := svc.buildOpenAIResponsesWSURL(account)
				require.NoError(t, err)
				svc.codexCookies.Store(account, logicalURL, http.Header{"Set-Cookie": []string{"__cflb=prior-http; Path=/; Secure"}})
				inbound := codexWSIngressInbound()
				inbound.Set(openAICodexTurnStateHeader, "client-owned")
				var inputFrames []string
				for _, frame := range []string{codexWSTestFrame, codexWSSecondFrame} {
					frame, err = sjson.Set(frame, "client_metadata.x-codex-turn-metadata", `{"model":"stale-model","reasoning_effort":"ultra"}`)
					require.NoError(t, err)
					inputFrames = append(inputFrames, frame)
				}
				runCodexWSIngress(t, svc, account, inbound, inputFrames)
				h := rawRelayRecv(t, headers)
				require.Contains(t, h.Get("Cookie"), "__cflb=prior-http")
				require.NotContains(t, h.Get("User-Agent"), "Go-http-client")
				if enabled {
					require.Empty(t, h.Get(openAICodexTurnStateHeader))
				}
				for turn := 0; turn < 2; turn++ {
					frame := rawRelayRecv(t, frames)
					require.Equal(t, "response.create", gjson.GetBytes(frame, "type").String())
					requireCodexGuardianCreditsRequested(t, frame, enabled)
					require.NotContains(t, string(frame), manual)
					if enabled {
						metadata := gjson.GetBytes(frame, "client_metadata.x-codex-turn-metadata").String()
						require.Equal(t, gjson.GetBytes(frame, "model").String(), gjson.Get(metadata, "model").String())
						require.Equal(t, "ultra", gjson.Get(metadata, "reasoning_effort").String())
						require.NotEmpty(t, gjson.GetBytes(frame, "client_metadata."+codexWSStreamRequestStartKey).String())
						requireCodexFieldOrder(t, topLevelKeys(t, frame), codexWantWSCreateOrder)
					}
				}
				probe := http.Header{}
				svc.codexCookies.Attach(account, logicalURL, probe)
				require.Contains(t, probe.Get("Cookie"), "__oailb=ws-route")
				t.Log("receiver verified real WS upgrade and 2 response.create frames; cookie response stored; injection remains disabled")
			})
		}
	}
}
