# Tencent Gongfeng PR screenshots

Review evidence for the Tencent Gongfeng integration in multica-ai/multica.

- Before: unmodified official main c8d6a1fad03d20a020ca90d1e50257d9f060ee87.
- After: integration patch SHA-256 9812fe4916f9143706bbda743558f67d3943390f8b8c99224589129871f60458.
- Images 01–07: Chromium 1280 x 720. Synthetic test data; the connection form token is empty.
- Images 08–09: native macOS arm64 Electron 39.8.7, built main/preload/renderer, real Multica API/PostgreSQL, and a deterministic local Gongfeng test service.
- External Gongfeng/public callback delivery was not part of the desktop run. Agent execution was disabled at test IPC boundaries.

This branch contains review assets only; images are excluded from the feature PR's source diff.
