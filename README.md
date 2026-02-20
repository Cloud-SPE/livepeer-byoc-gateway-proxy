# Gateway Proxy

A general-purpose proxy that sits in front of the Livepeer Gateway, exposing OpenAI-compatible, Cohere-compatible API and Video Generation endpoints.

Clients send standard API requests to the proxy. The proxy injects the required Livepeer BYOC capability headers, forwards the request to the gateway, and filters/fixes responses before returning them to the client.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/chat/completions` | OpenAI chat completions (streaming supported) |
| `POST` | `/v1/images/generations` | OpenAI image generation |
| `POST` | `/v1/embeddings` | OpenAI embeddings |
| `POST` | `/v1/rerank` | Cohere-compatible reranking |
| `POST` | `/v1/video/pipeline/generations` | Video pipeline generation (async, returns job ID) |
| `POST` | `/v1/video/pipeline/generations/status` | Video pipeline status polling |
| `GET`  | `/healthz` | Health check |

## Environment Variables

| Variable | Default | Description                          |
|----------|---------|--------------------------------------|
| `PROXY_ADDR` | `:8090` | Address the proxy listens on         |
| `GATEWAY_URL` | `http://gateway:9935` | Livepeer Gateway URL                 |
| `CHAT_COMPLETIONS_CAPABILITY` | `openai-chat-completions` | Capability name for chat completions |
| `IMAGE_GENERATION_CAPABILITY` | `openai-image-generation` | Capability name for image generation |
| `TEXT_EMBEDDINGS_CAPABILITY` | `openai-text-embeddings` | Capability name for embeddings       |
| `RERANK_CAPABILITY` | `cohere-rerank` | Capability name for reranking        |
| `VIDEO_PIPELINE_GENERATION_CAPABILITY` | `video-pipeline-generation` | Capability name for video pipeline   |
| `CHAT_COMPLETIONS_TIMEOUT_SECONDS` | `120` | Chat completions request timeout     |
| `IMAGE_GENERATION_TIMEOUT_SECONDS` | `120` | Image generation request timeout     |
| `TEXT_EMBEDDINGS_TIMEOUT_SECONDS` | `30` | Embeddings request timeout           |
| `RERANK_TIMEOUT_SECONDS` | `30` | Rerank request timeout               |
| `VIDEO_PIPELINE_GENERATION_TIMEOUT_SECONDS` | `900` | Video pipeline request timeout       |

## How It Works

Each incoming request is translated into a Livepeer Gateway call:

1. The proxy receives a standard API request (e.g. `POST /v1/chat/completions`).
2. It constructs a base64-encoded `Livepeer` header containing the capability name, a request marker, and the timeout:
   ```json
   {
     "request": "{\"run\":\"<capability>\"}",
     "parameters": "{\"orchestrators\":{\"include\":[],\"exclude\":[]}}",
     "capability": "<capability>",
     "timeout_seconds": 120
   }
   ```
3. The request body is forwarded unchanged to the gateway at `/process/request/<original-path>`.
4. The `Authorization` header is stripped (auth is handled externally, e.g. by Traefik).
5. On the response side, Livepeer-specific headers (`Livepeer-Balance`, `X-Metadata`, `X-Orchestrator-Url`) are removed.
6. For streaming (SSE) chat completions, non-OpenAI events injected by the gateway (e.g. balance metadata) are filtered out so they don't break OpenAI SDK parsers.

## Building & Running

### Docker (via build.sh)

```bash
# Local build, tag: latest
./build.sh

# Custom tag
TAG=v1.0.0 ./build.sh

# With registry prefix
REGISTRY=myregistry.example.com TAG=v1.0.0 ./build.sh

# Build and push
REGISTRY=myregistry.example.com PUSH=true ./build.sh
```

### Docker (manual)

```bash
docker build -t gateway-proxy .
docker run -p 8090:8090 \
  -e GATEWAY_URL=http://your-gateway:9935 \
  gateway-proxy
```

### Local Go build

```bash
go build -o gateway-proxy .
GATEWAY_URL=http://localhost:9935 ./gateway-proxy
```

## Testing

```bash
# Chat completions (non-streaming)
curl -sS http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model","stream":false,"messages":[{"role":"user","content":"Hello"}]}'

# Chat completions (streaming)
curl -N http://localhost:8090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model","stream":true,"messages":[{"role":"user","content":"Hello"}]}'

# Image generation
curl -sS http://localhost:8090/v1/images/generations \
  -H "Content-Type: application/json" \
  -d '{"model":"your-image-model","prompt":"A cat on the beach","n":1,"size":"1024x1024"}'

# Embeddings
curl -sS http://localhost:8090/v1/embeddings \
  -H "Content-Type: application/json" \
  -d '{"model":"your-embeddings-model","input":"Hello world"}'

# Rerank
curl -sS http://localhost:8090/v1/rerank \
  -H "Content-Type: application/json" \
  -d '{"model":"your-rerank-model","query":"search query","documents":["doc1","doc2"]}'

# Health check
curl http://localhost:8090/healthz
```

## Extending for New Capabilities

To add support for a new BYOC capability:

1. Add environment variables for the capability name and timeout.
2. Register a new `HandleFunc` route that constructs the appropriate `Livepeer` header and forwards to the gateway.
3. Register the capability on the orchestrator so the gateway can route to the runner.

## License

[MIT](LICENSE)
