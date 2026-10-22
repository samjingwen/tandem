# Attribution

Tandem adapts coordination ideas from [Kun Chen's Firstmate](https://github.com/kunchenguid/firstmate).
The inspected revision is `3847411b9a73f6553e172a6aefe5dff05e7aa178` (2026-10-21).
The inspected material includes its README, AGENTS.md, architecture, supervision engine, watcher, and OpenCode watcher plugin.
Useful ideas include durable ownership, event-driven supervision, separate worker checkouts, and reconciliation rather than treating wake events as current state.
Tandem is an independent implementation, not installation of the Firstmate distribution.
No Firstmate implementation code is copied into the initial engine.
Its V1 OpenCode watcher is not compatible with the V2 plugin interface.

Firstmate's MIT notice is retained here for attribution:

```text
MIT License

Copyright (c) 2026 Kun Chen

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

The OpenCode HTTP contract is inspected at V2 `2.1.0`, upstream revision `60c68d145a14127ce633e26aa5fdfb882a7baa6d`.
Dependency licenses remain those of their respective modules and npm packages.
SQLite's C implementation is public domain; its modernc Go port and dependencies retain their own notices.
