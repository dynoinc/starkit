# Starkit

[![Build Status](https://github.com/dynoinc/starkit/actions/workflows/test.yml/badge.svg)](https://github.com/dynoinc/starkit/actions)
[![Go Reference](https://pkg.go.dev/badge/github.com/dynoinc/starkit.svg)](https://pkg.go.dev/github.com/dynoinc/starkit)

Go toolkit to help build AI apps all on top of PostgreSQL.

## Features

* **Deterministic & durable workflows**
* **RAG store that provides lexical and semantic search over documents**
* **Helper to integrate multiple MCP servers**

## Quick Start

Look at [`assistant.star`](cmd/termichat/assistant.star) as an example of AI business logic and how to
integrate it with in your surfaces like [`terminal`](cmd/termichat/main.go).

Termichat shows you how to - 

- Setup workflow/ragstore/MCP servers for your app. 
- How to build a per-user memory system in < 50 lines of code.
- How to integrate deterministic workflows in an LLM powered app. 

You can run the example with:

```bash
DATABASE_URL=...
OPENAI_API_KEY=...
go run ./cmd/termichat/
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Security

Please see [SECURITY.md](SECURITY.md) for security policy and reporting guidelines. 
