# syntax=docker/dockerfile:1.7

FROM golang:1.24-bookworm AS go-builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY coreutils ./coreutils
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/go-core-mcp .

FROM ghcr.io/ggml-org/llama.cpp:server AS llama-runtime

FROM debian:bookworm-slim AS model-fetch
ARG DOWNLOAD_MODEL=0
ARG MODEL_URL="https://huggingface.co/unsloth/Qwen2.5-Coder-7B-Instruct-128K-GGUF/resolve/main/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf"
ARG MODEL_FILENAME="Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf"
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /models
RUN --mount=type=secret,id=hf_token \
    set -eu; \
    if [ "$DOWNLOAD_MODEL" = "1" ]; then \
      token=""; \
      if [ -f /run/secrets/hf_token ]; then token="$(cat /run/secrets/hf_token)"; fi; \
      if [ -n "$token" ]; then \
        curl -fL --retry 5 --retry-delay 2 --retry-all-errors --oauth2-bearer "$token" "$MODEL_URL" -o "/models/$MODEL_FILENAME"; \
      else \
        curl -fL --retry 5 --retry-delay 2 --retry-all-errors "$MODEL_URL" -o "/models/$MODEL_FILENAME"; \
      fi; \
    fi

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

ENV LLAMA_SERVER_HOST=127.0.0.1 \
    LLAMA_SERVER_PORT=8080 \
    LLAMA_MODEL_FILE=Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf \
    LLAMA_MODEL_NAME=Qwen2.5-Coder-7B-Instruct-Q4_K_M \
    LLAMA_CTX_SIZE=8192 \
    LLAMA_THREADS=0 \
    LLAMA_N_GPU_LAYERS=0 \
    LLAMA_STARTUP_TIMEOUT=180 \
    LD_LIBRARY_PATH=/opt/llama

COPY --from=go-builder /out/go-core-mcp /usr/local/bin/go-core-mcp
COPY --from=llama-runtime /app /opt/llama
COPY --from=model-fetch /models/ /models/
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
