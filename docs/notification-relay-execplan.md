# Historical notification relay plan

The original push-capability implementation has been replaced by the
[publisher-owned subscriber relay](notification-relay.md) and its
[SQL storage contract](notification-relay-sql.md).

The daemon retains App Attest as its default device-authentication module,
provider destination policy, signed installation management, durable replay
protection, token-revision fencing and installation quiet periods. It now
pulls delegated producer feeds and commits local jobs and its checkpoint before
remote ACK. Publishers configure their own sources, credentials and delivery.

The retired Bolt implementation, push/verification HTTP handlers, capability
DTOs and legacy-only tests are removed. Management proof/audience/counter tests
and anonymous challenge restart now use the same SQL store as the daemon.
Provider/presentation tests remain, alongside the SQL job, token recovery,
quota and subscriber ingress suites. There is one production state owner.

No importer or dual store is provided. This is a coordinated source cutover
requiring fresh publisher enrollment; it is not an automatic data conversion.
Source review and local SQL/provider acceptance do not prove device execution.
