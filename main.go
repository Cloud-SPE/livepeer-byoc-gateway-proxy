package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type livepeerHeader struct {
	Request        string `json:"request"`
	Capability     string `json:"capability"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func main() {
	addr := env("PROXY_ADDR", ":8090")
	gatewayURL := env("GATEWAY_URL", "http://gateway:9935")
	capability := env("CHAT_COMPLETIONS_CAPABILITY", "openai-chat-completions")
	imageCapability := env("IMAGE_GENERATION_CAPABILITY", "openai-image-generation")
	embeddingsCapability := env("TEXT_EMBEDDINGS_CAPABILITY", "openai-text-embeddings")
	rerankCapability := env("RERANK_CAPABILITY", "cohere-rerank")
	videoGenerationCapability := env("VIDEO_GENERATION_CAPABILITY", "video-generation")
	timeoutSeconds := envInt("CHAT_COMPLETIONS_TIMEOUT_SECONDS", 120)
	imageTimeoutSeconds := envInt("IMAGE_GENERATION_TIMEOUT_SECONDS", 120)
	embeddingsTimeoutSeconds := envInt("TEXT_EMBEDDINGS_TIMEOUT_SECONDS", 30)
	rerankTimeoutSeconds := envInt("RERANK_TIMEOUT_SECONDS", 30)
	videoPipelineTimeoutSeconds := envInt("VIDEO_GENERATION_TIMEOUT_SECONDS", 900)

	target := strings.TrimRight(gatewayURL, "/") + "/process/request/v1/chat/completions"
	imageTarget := strings.TrimRight(gatewayURL, "/") + "/process/request/v1/images/generations"
	embeddingsTarget := strings.TrimRight(gatewayURL, "/") + "/process/request/v1/embeddings"
	rerankTarget := strings.TrimRight(gatewayURL, "/") + "/process/request/v1/rerank"
	videoGenerationTarget := strings.TrimRight(gatewayURL, "/") + "/process/request/v1/video/generations"

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{Transport: transport}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Proxy should be streaming-friendly; optionally use a hard timeout
		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
		defer cancel()

		const maxBody = 5 << 20 // 5MB
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "failed to create gateway request", http.StatusBadGateway)
			return
		}
		req.ContentLength = int64(len(bodyBytes))

		// Copy content-type and accept (keep it simple)
		copyHeader(req.Header, r.Header, []string{"Content-Type", "Accept"})

		// Strip client auth headers (Traefik handles auth/rate limit)
		req.Header.Del("Authorization")

		// Build Livepeer header
		lp := map[string]any{
			"request":         `{"run":"` + capability + `"}`,
			"parameters":      `{"orchestrators":{"include":[],"exclude":[]}}`,
			"capability":      capability,
			"timeout_seconds": timeoutSeconds,
		}

		b, _ := json.Marshal(lp)
		req.Header.Set("Livepeer", base64.StdEncoding.EncodeToString(b))
		decoded, _ := base64.StdEncoding.DecodeString(req.Header.Get("Livepeer"))
		log.Printf("sending to gateway: url=%s content_len=%d livepeer=%s",
			target, len(bodyBytes), string(decoded),
		)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "gateway request failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		copyAllHeaders(w.Header(), resp.Header)

		// Fix: The Livepeer gateway may pass through an incorrect
		// Content-Type (text/plain). Override it at the proxy layer as
		// a safety net — this is what clients actually see.
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}

		// Strip Livepeer-specific headers that aren't part of the OpenAI API
		w.Header().Del("Livepeer-Balance")
		w.Header().Del("X-Metadata")
		w.Header().Del("X-Orchestrator-Url")

		w.WriteHeader(resp.StatusCode)

		// For SSE responses, filter out non-OpenAI events injected by
		// the Livepeer gateway (e.g. {"balance": ...}). These events
		// lack the "choices" field and crash OpenAI SDK parsers.
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			streamSSEFiltered(w, resp.Body)
		} else {
			streamResponse(w, resp.Body)
		}
	})

	// Image generation endpoint — routes to image runner via BYOC
	mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, time.Duration(imageTimeoutSeconds)*time.Second)
		defer cancel()

		const maxBody = 1 << 20 // 1MB
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, imageTarget, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "failed to create gateway request", http.StatusBadGateway)
			return
		}
		req.ContentLength = int64(len(bodyBytes))

		copyHeader(req.Header, r.Header, []string{"Content-Type", "Accept"})
		req.Header.Del("Authorization")

		// Build Livepeer header for image capability
		lp := map[string]any{
			"request":         `{"run":"` + imageCapability + `"}`,
			"parameters":      `{"orchestrators":{"include":[],"exclude":[]}}`,
			"capability":      imageCapability,
			"timeout_seconds": imageTimeoutSeconds,
		}

		b, _ := json.Marshal(lp)
		req.Header.Set("Livepeer", base64.StdEncoding.EncodeToString(b))
		log.Printf("image gen request to gateway: url=%s content_len=%d", imageTarget, len(bodyBytes))

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "gateway request failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		copyAllHeaders(w.Header(), resp.Header)
		// Ensure Content-Type is application/json so OpenAI SDK parses correctly
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)

		// Image generation is not streaming — just copy the full response
		io.Copy(w, resp.Body)
	})

	// Embeddings endpoint — routes to embeddings runner via BYOC
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, time.Duration(embeddingsTimeoutSeconds)*time.Second)
		defer cancel()

		const maxBody = 1 << 20 // 1MB
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, embeddingsTarget, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "failed to create gateway request", http.StatusBadGateway)
			return
		}
		req.ContentLength = int64(len(bodyBytes))

		copyHeader(req.Header, r.Header, []string{"Content-Type", "Accept"})
		req.Header.Del("Authorization")

		// Build Livepeer header for embeddings capability
		lp := map[string]any{
			"request":         `{"run":"` + embeddingsCapability + `"}`,
			"parameters":      `{"orchestrators":{"include":[],"exclude":[]}}`,
			"capability":      embeddingsCapability,
			"timeout_seconds": embeddingsTimeoutSeconds,
		}

		b, _ := json.Marshal(lp)
		req.Header.Set("Livepeer", base64.StdEncoding.EncodeToString(b))
		log.Printf("embeddings request to gateway: url=%s content_len=%d", embeddingsTarget, len(bodyBytes))

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "gateway request failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		copyAllHeaders(w.Header(), resp.Header)
		// Ensure Content-Type is application/json so OpenAI SDK parses correctly
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)

		// Embeddings are not streaming — just copy the full response
		io.Copy(w, resp.Body)
	})

	// Rerank endpoint — routes to rerank runner via BYOC
	mux.HandleFunc("/v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, time.Duration(rerankTimeoutSeconds)*time.Second)
		defer cancel()

		const maxBody = 1 << 20 // 1MB
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rerankTarget, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "failed to create gateway request", http.StatusBadGateway)
			return
		}
		req.ContentLength = int64(len(bodyBytes))

		copyHeader(req.Header, r.Header, []string{"Content-Type", "Accept"})
		req.Header.Del("Authorization")

		// Build Livepeer header for rerank capability
		lp := map[string]any{
			"request":         `{"run":"` + rerankCapability + `"}`,
			"parameters":      `{"orchestrators":{"include":[],"exclude":[]}}`,
			"capability":      rerankCapability,
			"timeout_seconds": rerankTimeoutSeconds,
		}

		b, _ := json.Marshal(lp)
		req.Header.Set("Livepeer", base64.StdEncoding.EncodeToString(b))
		log.Printf("rerank request to gateway: url=%s content_len=%d", rerankTarget, len(bodyBytes))

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "gateway request failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		copyAllHeaders(w.Header(), resp.Header)
		// Ensure Content-Type is application/json
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Livepeer-Balance")
		w.Header().Del("X-Metadata")
		w.Header().Del("X-Orchestrator-Url")
		w.WriteHeader(resp.StatusCode)

		// Rerank is not streaming — just copy the full response
		io.Copy(w, resp.Body)
	})

	// Video generation endpoint — starts async job, returns job_id
	mux.HandleFunc("/v1/video/generations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Use a short timeout for job submission (returns immediately)
		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		const maxBody = 1 << 20 // 1MB
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, videoGenerationTarget, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "failed to create gateway request", http.StatusBadGateway)
			return
		}
		req.ContentLength = int64(len(bodyBytes))

		copyHeader(req.Header, r.Header, []string{"Content-Type", "Accept"})
		req.Header.Del("Authorization")

		// Build Livepeer header for video pipeline capability
		lp := map[string]any{
			"request":         `{"run":"` + videoGenerationCapability + `"}`,
			"parameters":      `{"orchestrators":{"include":[],"exclude":[]}}`,
			"capability":      videoGenerationCapability,
			"timeout_seconds": videoPipelineTimeoutSeconds,
		}

		b, _ := json.Marshal(lp)
		req.Header.Set("Livepeer", base64.StdEncoding.EncodeToString(b))
		log.Printf("video generation request to gateway: url=%s content_len=%d", videoGenerationTarget, len(bodyBytes))

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "gateway request failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		copyAllHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Livepeer-Balance")
		w.Header().Del("X-Metadata")
		w.Header().Del("X-Orchestrator-Url")
		w.WriteHeader(resp.StatusCode)

		io.Copy(w, resp.Body)
	})

	// Video pipeline status endpoint — poll job progress
	videoPipelineStatusTarget := strings.TrimRight(gatewayURL, "/") + "/process/request/v1/video/generations/status"
	mux.HandleFunc("/v1/video/generations/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		const maxBody = 1 << 20
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, videoPipelineStatusTarget, bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "failed to create gateway request", http.StatusBadGateway)
			return
		}
		req.ContentLength = int64(len(bodyBytes))

		copyHeader(req.Header, r.Header, []string{"Content-Type", "Accept"})
		req.Header.Del("Authorization")

		lp := map[string]any{
			"request":         `{"run":"` + videoGenerationCapability + `"}`,
			"parameters":      `{"orchestrators":{"include":[],"exclude":[]}}`,
			"capability":      videoGenerationCapability,
			"timeout_seconds": 30,
		}

		b, _ := json.Marshal(lp)
		req.Header.Set("Livepeer", base64.StdEncoding.EncodeToString(b))

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "gateway request failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		copyAllHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Livepeer-Balance")
		w.Header().Del("X-Metadata")
		w.Header().Del("X-Orchestrator-Url")
		w.WriteHeader(resp.StatusCode)

		io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("OpenAI proxy listening on %s, gateway=%s, llm_capability=%s, image_capability=%s, embeddings_capability=%s, rerank_capability=%s, video_generation_capability=%s", addr, gatewayURL, capability, imageCapability, embeddingsCapability, rerankCapability, videoGenerationCapability)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func env(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	_, err := fmtSscanf(v, &n)
	if err != nil {
		return def
	}
	return n
}

// tiny helper to avoid importing fmt just for Sscanf overhead in this snippet’s spirit
func fmtSscanf(s string, out *int) (int, error) {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, io.ErrUnexpectedEOF
		}
		n = n*10 + int(ch-'0')
	}
	*out = n
	return 1, nil
}

func copyHeader(dst http.Header, src http.Header, keys []string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func copyAllHeaders(dst http.Header, src http.Header) {
	for k, vv := range src {
		if strings.EqualFold(k, "Connection") ||
			strings.EqualFold(k, "Keep-Alive") ||
			strings.EqualFold(k, "Proxy-Authenticate") ||
			strings.EqualFold(k, "Proxy-Authorization") ||
			strings.EqualFold(k, "TE") ||
			strings.EqualFold(k, "Trailer") ||
			strings.EqualFold(k, "Transfer-Encoding") ||
			strings.EqualFold(k, "Upgrade") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// streamSSEFiltered reads SSE events line-by-line and forwards only valid
// OpenAI chat completion chunks. The Livepeer gateway injects non-standard
// SSE events (e.g. `data: {"balance": ...}`) that lack the "choices" field.
// OpenAI SDK clients try to parse every `data:` line as a completion chunk
// and crash with "Cannot read properties of undefined (reading '0')" when
// they encounter these events.
func streamSSEFiltered(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	// Increase buffer for large SSE lines (e.g. long reasoning tokens)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()

		// Pass through empty lines (SSE event separators)
		if line == "" {
			_, _ = w.Write([]byte("\n"))
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}

		// For "data:" lines, check if it's a valid OpenAI chunk
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")

			// Always pass through [DONE]
			if payload == "[DONE]" {
				_, _ = w.Write([]byte(line + "\n"))
				if flusher != nil {
					flusher.Flush()
				}
				continue
			}

			// Parse and check for "choices" field — if absent, it's a
			// Livepeer-injected event (balance, metadata, etc.), skip it
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(payload), &obj); err == nil {
				if _, hasChoices := obj["choices"]; !hasChoices {
					log.Printf("filtered non-OpenAI SSE event: %s", payload)
					continue
				}
			}
		}

		// Forward the line as-is
		_, _ = w.Write([]byte(line + "\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func streamResponse(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
