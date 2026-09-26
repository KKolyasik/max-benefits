// Package contract is what the bot and the agent say to each other through
// Kafka: the topics, the messages and their Avro schemas.
//
// A message goes in the Confluent wire format: a zero byte, the ID of its
// schema in the Schema Registry and the Avro data. The schemas are in
// avro/*.avsc. Each is registered under the full name of its record
// (maxbenefits.Draft and so on), and the registry keeps every subject
// FULL_TRANSITIVE: a message written with any version of a schema reads with
// any other. So the bot and the agent can be deployed in any order, but a
// schema may change only in compatible ways:
//
//   - a new field needs a default;
//   - only a field with a default may be removed;
//   - a new enum symbol is fine: older readers see UNKNOWN instead.
//
// A service registers the schemas when it starts (Codec.Register), so an
// incompatible change fails the start, and the deploy rolls back.
package contract
