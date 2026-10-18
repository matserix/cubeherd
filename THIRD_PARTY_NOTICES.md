# Third-party notices

Cubeherd bundles the following components. Their licenses apply to those parts only.

| Component | Where | License |
|---|---|---|
| [htmx](https://htmx.org) 2.0.4 | `internal/web/static/htmx.min.js` | Zero-Clause BSD |
| [htmx SSE extension](https://github.com/bigskysoftware/htmx-extensions) 2.2.2 | `internal/web/static/sse.js` | Zero-Clause BSD |
| [IBM Plex Sans / Mono](https://github.com/IBM/plex) | `internal/web/static/fonts/plex*.woff2` | SIL Open Font License 1.1 (see `internal/web/static/fonts/OFL.txt`) |
| [Chakra Petch](https://github.com/m4rc1e/Chakra-Petch) | `internal/web/static/fonts/chakra-*.woff2` | SIL Open Font License 1.1 (see `internal/web/static/fonts/OFL.txt`) |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) | Go dependency (bcrypt) | BSD-3-Clause, reproduced below |

Cubeherd is not affiliated with Mojang Studios or Microsoft. "Minecraft" is a trademark of Mojang Synergies AB.
Cubeherd drives the [itzg/docker-minecraft-server](https://github.com/itzg/docker-minecraft-server) image (Apache-2.0) but does not include it.

## golang.org/x/crypto

```
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
