package wire

import (
	"errors"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// DeviceContext is the location and time zone the bridge reports on behalf of a
// station that never sends its own. The station keeps a "set_timezone" and a
// "set_location" directive unanswered, so weather and other local skills have
// nothing to work with. Local patch (not upstream): an empty value leaves the
// frame byte-for-byte untouched.
type DeviceContext struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Accuracy float64 `json:"accuracy"`
	Timezone string  `json:"timezone"`
}

// Enabled reports whether the frame should be patched at all.
func (c DeviceContext) Enabled() bool { return c.Located() || c.Timezone != "" }

// Located reports whether coordinates are configured. Zero is a real latitude,
// so the pair is required as a whole and never inferred from one value.
func (c DeviceContext) Located() bool { return c.Lat != 0 || c.Lon != 0 }

// ContextPatch reports which leaves were actually written. TimezoneSkipped
// means the frame carried no client info block, so the time zone was left alone
// instead of assembling a truncated one.
type ContextPatch struct {
	Location        bool
	Timezone        bool
	TimezoneSkipped bool
}

// AddDeviceContext patches the location and the time zone into an already
// encoded Alice input. Everything the station sent, including unknown fields
// and duplicate fields, is copied unchanged; only the two leaves are rewritten.
// The prefix ("AAPI") is optional and preserved.
func AddDeviceContext(raw []byte, kind string, c DeviceContext) ([]byte, ContextPatch, error) {
	var patch ContextPatch
	field, ok := inputField(kind)
	if !ok {
		return raw, patch, errors.New("not an Alice input")
	}
	prefix := 0
	if len(raw) > 4 && string(raw[:4]) == "AAPI" {
		prefix = 4
	}
	body := raw[prefix:]
	out := body
	if c.Located() {
		patched, err := patchMessage(out, []protowire.Number{1, field, 3, 2}, locationBytes(c))
		if err != nil {
			return raw, patch, err
		}
		out, patch.Location = patched, true
	}
	if c.Timezone != "" {
		if _, found := messageAt(out, []protowire.Number{1, field, 2}); found {
			patched, err := patchMessage(out, []protowire.Number{1, field, 2, 9}, []byte(c.Timezone))
			if err != nil {
				return raw, patch, err
			}
			out, patch.Timezone = patched, true
		} else {
			patch.TimezoneSkipped = true
		}
	}
	if !patch.Location && !patch.Timezone {
		return raw, patch, nil
	}
	result := make([]byte, 0, prefix+len(out))
	result = append(result, raw[:prefix]...)
	result = append(result, out...)
	return result, patch, nil
}

// inputField names the AliceRequestProto member of a client event.
func inputField(kind string) (protowire.Number, bool) {
	switch kind {
	case "TextInput":
		return 2, true
	case "VoiceInput":
		return 3, true
	case "SpeechInput":
		return 18, true
	}
	return 0, false
}

// locationBytes encodes a TLocation message: Lat and Lon are fixed64, Accuracy
// is fixed64 too. Only accuracy is conditional, because a zero accuracy is the
// "unknown" value while zero coordinates are a real place.
func locationBytes(c DeviceContext) []byte {
	var b []byte
	b = protowire.AppendFixed64(protowire.AppendTag(b, 1, protowire.Fixed64Type), math.Float64bits(c.Lat))
	b = protowire.AppendFixed64(protowire.AppendTag(b, 2, protowire.Fixed64Type), math.Float64bits(c.Lon))
	if c.Accuracy > 0 {
		b = protowire.AppendFixed64(protowire.AppendTag(b, 3, protowire.Fixed64Type), math.Float64bits(c.Accuracy))
	}
	return b
}

// patchMessage replaces the first occurrence of the leaf at path, or appends it
// when the parent message exists. A missing intermediate parent is an error: it
// is never better to rebuild one, because the frame would lose every field the
// station already put there.
func patchMessage(data []byte, path []protowire.Number, value []byte) ([]byte, error) {
	if len(path) == 1 {
		return replaceBytesField(data, path[0], value), nil
	}
	before, body, after, found := splitBytesField(data, path[0])
	if !found {
		return nil, errors.New("device context: parent field is missing or not a message")
	}
	patched, err := patchMessage(body, path[1:], value)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(before)+len(patched)+len(after)+8)
	out = append(out, before...)
	out = protowire.AppendBytes(protowire.AppendTag(out, path[0], protowire.BytesType), patched)
	return append(out, after...), nil
}

// replaceBytesField rewrites the first length-delimited field with the given
// number, or appends one when it is absent.
func replaceBytesField(data []byte, num protowire.Number, value []byte) []byte {
	before, _, after, found := splitBytesField(data, num)
	if !found {
		out := make([]byte, 0, len(data)+len(value)+8)
		out = append(out, data...)
		return protowire.AppendBytes(protowire.AppendTag(out, num, protowire.BytesType), value)
	}
	out := make([]byte, 0, len(before)+len(value)+len(after)+8)
	out = append(out, before...)
	out = protowire.AppendBytes(protowire.AppendTag(out, num, protowire.BytesType), value)
	return append(out, after...)
}

// splitBytesField cuts the first length-delimited field with the given number
// into the bytes before it, its payload and the bytes after it.
func splitBytesField(data []byte, num protowire.Number) (before, body, after []byte, found bool) {
	for offset := 0; offset < len(data); {
		n, kind, size := protowire.ConsumeTag(data[offset:])
		if size < 0 {
			return nil, nil, nil, false
		}
		length := protowire.ConsumeFieldValue(n, kind, data[offset+size:])
		if length < 0 {
			return nil, nil, nil, false
		}
		end := offset + size + length
		if n == num && kind == protowire.BytesType {
			value, consumed := protowire.ConsumeBytes(data[offset+size:])
			if consumed < 0 {
				return nil, nil, nil, false
			}
			return data[:offset], value, data[end:], true
		}
		offset = end
	}
	return nil, nil, nil, false
}

// messageAt returns the payload of the message field at path, if it exists.
func messageAt(data []byte, path []protowire.Number) ([]byte, bool) {
	current := data
	for i, num := range path {
		_, body, _, found := splitBytesField(current, num)
		if !found {
			return nil, false
		}
		if i == len(path)-1 {
			return body, true
		}
		current = body
	}
	return nil, false
}
