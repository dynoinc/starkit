# Starflow

[![Build Status](https://github.com/dynoinc/starflow/actions/workflows/test.yml/badge.svg)](https://github.com/dynoinc/starflow/actions)
[![Go Reference](https://pkg.go.dev/badge/github.com/dynoinc/starflow.svg)](https://pkg.go.dev/github.com/dynoinc/starflow)

A workflow engine for Go that enables deterministic and resumable workflow execution using Starlark scripting.

## Features

### Deterministic & Durable Workflows
Write workflows in Starlark (Python-like syntax) that are deterministic and can be replayed from any point with full durability guarantees. 
Every execution step is recorded and can be resumed exactly where it left off.

### Pluggable Storage Backends
Any store that implements the simple Store interface can be used as a backend. The interface uses append-only operations with optimistic concurrency control. 

## Installation

```bash
go get github.com/dynoinc/starflow
```

## Quick Start

For a complete working example, please see the [`example_test.go`](example_test.go) file in this repository. It demonstrates how to:

- Create an in-memory store
- Register functions for use in workflows
- Define workflow scripts using Starlark
- Execute workflows and retrieve results

You can run the example with:

```bash
go test -run Example
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Security

Please see [SECURITY.md](SECURITY.md) for security policy and reporting guidelines. 
