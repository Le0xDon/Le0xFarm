// Package protocol distinguishes wire protocol and document schema versions.
package protocol

type ProtocolVersion uint32
type SchemaVersion uint32

// Foundation versions; no wire protocol or protobuf messages exist yet.
const CurrentProtocolVersion ProtocolVersion = 1
const CurrentSchemaVersion SchemaVersion = 1
