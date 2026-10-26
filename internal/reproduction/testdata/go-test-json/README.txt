Recorded with go1.26 (golang:1.26 image) unless named synth-*.json.

module-fetch-get.json, module-fetch-proxyconnect.json and
module-fetch-unrecognized.json are real runs of a reproduction that imports a
module go.mod requires and the sandbox cannot fetch (GOPROXY at a host that
does not resolve; HTTPS_PROXY at a closed port; GOPROXY=direct).

synth-*.json are synthesised from module-fetch-get.json's event shape, each
with one go command message of the kind its name says (a module@version
prefix, Get "http..., x509:, tls:, proxyconnect, unrecognized import path,
verifying module, connection reset, TLS handshake timeout), one marker per
file, because Docker here cannot produce each failure on its own.
