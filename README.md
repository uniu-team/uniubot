# SkiBot
✨ A simple, easy to get started bot framework based on Go and the OneBot-V11 protocol ✨

## ⚙️ Run
### by source code

```
git clone https://github.com/unify-z/skibot.git
cd skibot
go mod download
cp config/config.yml.example config/config.yml
make build            //builds ./bin/uniubot and the Go plugins
./bin/uniubot         //production mode
go run ./cmd/uniubot  //development mode
```
The dashboard is a Vue 3 SPA kept from the original implementation. Build it once with
`make frontend` (requires Node.js); the Go binary serves `frontend/dist` afterwards.

For frontend development set `dashboard.debug: true` in `config/config.yml`: the host starts
the Vite dev server by itself (`dashboard.dev_port`, default 3000) and proxies the dashboard to
it, so the UI hot-reloads. Run `cd frontend && npm install` once beforehand, or point the host
at an already running dev server with `--dev-server http://host:port`.

### by release(to do)
```
unzip windows-amd64.zip
cd windows-amd64
./uniubot.exe
```
## 📖 License
The project is open-sourced under the Apache-2.0 license.

## 🔌 Plugins
Plugins run as **separate processes** and talk to the host over JSON-RPC 2.0 (NDJSON on
stdin/stdout), so they can be written in Go, Python, Lua or any language that can read a
line and write a line.

Reference implementations live in `plugins/examples/{go,python,lua}`; scaffold a new plugin
with `./scripts/create-plugin.sh`.

## 📚 Docs
- [Plugin system](docs/plugin-system.md) — manifest, lifecycle, permissions, SDKs
- [Plugin protocol](docs/plugin-protocol.md) — the JSON-RPC wire specification
- [Architecture](docs/architecture.md) — Go module layout and design decisions
- [Migration notes](docs/migration-notes.md) — differences from the TypeScript version

## 📍 ToDo
- [ ] Plugin Market
- [x] Documentation
