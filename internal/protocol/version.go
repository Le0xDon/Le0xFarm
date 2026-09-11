// Package protocol distinguishes wire protocol and document schema versions.
package protocol

type ProtocolVersion uint32
type SchemaVersion uint32

// Foundation versions, explicitly carried by the v1 protocol hello messages.
const CurrentProtocolVersion ProtocolVersion = 6
const CurrentSchemaVersion SchemaVersion = 1
