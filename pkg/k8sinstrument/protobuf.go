package k8sinstrument

import (
	"bytes"
	"errors"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// protobufMagic prefixes every Kubernetes protobuf-encoded object.
var protobufMagic = []byte{0x6b, 0x38, 0x73, 0x00} // "k8s\x00"

var errNotObject = errors.New("not a Kubernetes protobuf object")

// parseProtobufMetadata extracts kind and object metadata from a Kubernetes
// protobuf response without decoding the object. The envelope is a
// runtime.Unknown message:
//
//	Unknown { TypeMeta typeMeta = 1; bytes raw = 2; ... }
//	TypeMeta { string apiVersion = 1; string kind = 2; }
//
// and in every top-level built-in object field 1 is ObjectMeta:
//
//	ObjectMeta { ... string uid = 5; string resourceVersion = 6; int64 generation = 7; ... }
func parseProtobufMetadata(b []byte) (objectMeta, error) {
	var m objectMeta
	if !bytes.HasPrefix(b, protobufMagic) {
		return m, errNotObject
	}
	unknown := b[len(protobufMagic):]
	var raw []byte
	err := fields(unknown, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			return fields(v, func(n protowire.Number, t protowire.Type, s []byte, _ uint64) error {
				if n == 2 && t == protowire.BytesType {
					m.Kind = string(s)
				}
				return nil
			})
		case num == 2 && typ == protowire.BytesType:
			raw = v
		}
		return nil
	})
	if err != nil || raw == nil {
		return objectMeta{}, errNotObject
	}
	err = fields(raw, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		return fields(v, func(n protowire.Number, t protowire.Type, s []byte, u uint64) error {
			switch {
			case n == 5 && t == protowire.BytesType:
				m.Metadata.UID = string(s)
			case n == 6 && t == protowire.BytesType:
				m.Metadata.ResourceVersion = string(s)
			case n == 7 && t == protowire.VarintType && u <= math.MaxInt64:
				m.Metadata.Generation = int64(u)
			}
			return nil
		})
	})
	if err != nil {
		return objectMeta{}, err
	}
	return m, nil
}

// fields iterates over the top-level fields of a protobuf message.
func fields(b []byte, fn func(protowire.Number, protowire.Type, []byte, uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			if err := fn(num, typ, v, 0); err != nil {
				return err
			}
			b = b[m:]
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			if err := fn(num, typ, nil, v); err != nil {
				return err
			}
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			b = b[m:]
		}
	}
	return nil
}
