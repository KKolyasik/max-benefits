package contract

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sync"

	"github.com/hamba/avro/v2"
	"github.com/twmb/franz-go/pkg/sr"
)

// Codec writes and reads the messages of this package in the Confluent wire
// format, with their schemas in a Schema Registry. It is safe for concurrent
// use.
type Codec struct {
	reg    *sr.Client
	header sr.ConfluentHeader

	mu sync.Mutex
	// ids are the registry IDs of our schemas, known after Register.
	ids map[reflect.Type]int
	// readers are other versions of a schema, resolved into our types.
	readers map[reader]avro.Schema
}

type reader struct {
	typ reflect.Type
	id  int
}

// NewCodec returns a codec over the registry.
func NewCodec(reg *sr.Client) *Codec {
	return &Codec{reg: reg, ids: map[reflect.Type]int{}, readers: map[reader]avro.Schema{}}
}

// Register keeps every subject FULL_TRANSITIVE and registers our schemas. A
// schema incompatible with a registered version fails here; registering the
// same schema again is harmless.
func (c *Codec) Register(ctx context.Context) error {
	for _, f := range files {
		s := schemas[f.typ]
		compat := sr.SetCompatibility{Level: sr.CompatFullTransitive}
		for _, res := range c.reg.SetCompatibility(ctx, compat, s.subject) {
			if res.Err != nil {
				return fmt.Errorf("set compatibility of %s: %w", s.subject, res.Err)
			}
		}
		id, err := c.reg.RegisterSchema(ctx, s.subject, sr.Schema{Schema: s.json, Type: sr.TypeAvro}, -1, -1)
		if err != nil {
			return fmt.Errorf("register %s: %w", s.subject, err)
		}
		c.mu.Lock()
		c.ids[f.typ] = id
		c.mu.Unlock()
	}
	return nil
}

// Encode writes v, a message of this package or a pointer to one.
func (c *Codec) Encode(v any) ([]byte, error) {
	typ := reflect.TypeOf(v)
	if typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	s, ok := schemas[typ]
	if !ok {
		return nil, fmt.Errorf("%T is not a message of the contract", v)
	}
	c.mu.Lock()
	id, ok := c.ids[typ]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("the schema of %T is not registered: call Register first", v)
	}
	b, err := c.header.AppendEncode(nil, id, nil)
	if err != nil {
		return nil, err
	}
	data, err := avro.Marshal(s.avro, v)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", v, err)
	}
	return append(b, data...), nil
}

// Decode reads into v, a pointer to a message of this package, a message
// written with any version of its schema: the older or newer fields are
// resolved by the rules of Avro.
func (c *Codec) Decode(ctx context.Context, data []byte, v any) error {
	typ := reflect.TypeOf(v)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return fmt.Errorf("decode into %T: need a pointer", v)
	}
	typ = typ.Elem()
	s, ok := schemas[typ]
	if !ok {
		return fmt.Errorf("%T is not a message of the contract", v)
	}
	id, payload, err := c.header.DecodeID(data)
	if err != nil {
		return fmt.Errorf("decode %T: %w", v, err)
	}
	r, err := c.reader(ctx, typ, s, id)
	if err != nil {
		return err
	}
	if err := avro.Unmarshal(r, payload, v); err != nil {
		return fmt.Errorf("decode %T with schema %d: %w", v, id, err)
	}
	return nil
}

// reader returns the schema to read the version id of the schema s with.
func (c *Codec) reader(ctx context.Context, typ reflect.Type, s *schema, id int) (avro.Schema, error) {
	c.mu.Lock()
	own, registered := c.ids[typ]
	r, resolved := c.readers[reader{typ, id}]
	c.mu.Unlock()
	switch {
	case registered && own == id:
		return s.avro, nil
	case resolved:
		return r, nil
	}

	written, err := c.reg.SchemaByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("schema %d of %s: %w", id, s.subject, err)
	}
	if written.Type != sr.TypeAvro {
		return nil, fmt.Errorf("schema %d of %s is %s, not Avro", id, s.subject, written.Type)
	}
	// A cache of its own: another version may define our names differently.
	writer, err := avro.ParseWithCache(written.Schema, "", &avro.SchemaCache{})
	if err != nil {
		return nil, fmt.Errorf("schema %d of %s: %w", id, s.subject, err)
	}
	r, err = avro.NewSchemaCompatibility().Resolve(s.avro, writer)
	if err != nil {
		return nil, fmt.Errorf("schema %d cannot be read as %s: %w", id, s.subject, err)
	}
	c.mu.Lock()
	c.readers[reader{typ, id}] = r
	c.mu.Unlock()
	return r, nil
}

// Temporary tells an error of Decode that passes, as the registry is down
// or failing, from a message that can never be read.
func Temporary(err error) bool {
	var resp *sr.ResponseError
	if errors.As(err, &resp) {
		return resp.StatusCode >= 500
	}
	var netErr *url.Error
	return errors.As(err, &netErr)
}
