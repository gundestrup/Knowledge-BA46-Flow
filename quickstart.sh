#!/bin/sh
set -eu
mkdir -p data knowledge
if [ ! -f config.local.json ]; then
  cp config.json config.local.json
  echo "Created config.local.json. Edit repository_path and ollama_model before running."
fi
echo "Build: make build"
echo "Run:   ./KBM-flow run --config config.local.json"
