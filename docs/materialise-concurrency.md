# Materialisation concurrency boundaries

A service without `port_var` is admitted once per repository path and service name. With `port_var`, the workspace name is also part of its scope. Concurrent calls in the same Continuum home take an advisory file lock around the cache check, service command, and successful cache marker. Separate scopes can proceed concurrently. The shared JSON cache uses a second, short lock around read-modify-write and an atomic file replacement, so independent entries are retained.

The service lock waits at most 11 minutes, covering the existing 10-minute service command timeout; cache updates wait at most five seconds. A timeout is an error. A failed command is not marked successful. If the command succeeds but writing its marker fails, the error explicitly asks the operator to inspect before retrying. Dry runs return before lock creation or execution and leave the filesystem unchanged.

This is concurrent admission during normal operation. A crash between an external effect and its cache marker can still leave an uncertain result. It does not provide crash-safe exactly-once execution. Existing unsupervised cache entries remember successful starts; they do not prove the service is still alive. Supervised services check their declared port, allowing the configured settle window (30 seconds by default), before accepting a cached start or admitting a restart. Repository and workspace identities retain their existing path/name spelling.

Command cache hits require an unchanged cache-key file, the same expanded command, and the same workspace path. Another workspace still creates its own artifacts. Command execution and lifecycle hooks are not serialized by this service lock; only their shared cache writes are protected. No whole-workspace transaction is promised.

Lock files remain in the private home to avoid replacing a locked inode while a waiter holds it. Cache files are private and atomically replaced after syncing their contents. Power-loss durability of the directory entry and native Linux execution require separate validation.
