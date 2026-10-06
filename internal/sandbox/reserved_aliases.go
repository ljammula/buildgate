package sandbox

// ReservedWorkerAliases lists every internal-network hostname this package
// reserves for a sandbox's own sidecars -- a worker reaches the registry
// proxy by exactly this name (see registryProxyWorkerHost), so nothing else
// sharing a worker's internal network may claim it, whether as its own
// service/container name or as a `networks: aliases:` entry impersonating
// it. Exported so internal/composeservices can reject a target-repo compose
// file that tries either without keeping its own hardcoded copy of this
// list -- see that package's own reservedAliases for the check this feeds.
var ReservedWorkerAliases = []string{registryProxyWorkerHost}
