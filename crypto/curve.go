package crypto

import (
	"bytes"
	"crypto"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
)

var (
	ErrInvalidPoint     = errors.New("marshaled point was invalid")
	ErrNoPointFound     = errors.New("hash_to_curve failed to find a point")
	ErrPointOffCurve    = errors.New("point is not on curve")
	ErrUnspecifiedCurve = errors.New("must specify an elliptic curve")
	ErrCommSanityCheck  = errors.New("commitment does not match key")
)

type Point struct {
	Curve elliptic.Curve
	X, Y  *big.Int
}

func (p *Point) IsOnCurve() bool {
	return p.Curve.IsOnCurve(p.X, p.Y)
}

func (p *Point) MarshalJSON() ([]byte, error) {
	byteRepr := p.Marshal()
	jsonRepr, err := json.Marshal(byteRepr)
	if err != nil {
		return nil, err
	}
	return jsonRepr, nil
}

func (p *Point) UnmarshalJSON(data []byte) error {
	var byteRepr []byte
	err := json.Unmarshal(data, byteRepr)
	if err != nil {
		return err
	}
	err = p.Unmarshal(p.Curve, data)
	if err != nil {
		return err
	}
	return nil
}

// Marshal calls through to elliptic.Marshal using the Curve field of the
// receiving Point. This produces an uncompressed marshaling as specified in
// SEC1 2.3.3.
func (p *Point) Marshal() []byte {
	return elliptic.Marshal(p.Curve, p.X, p.Y)
}

// Unmarshal interprets SEC1 2.3.4 compressed points in addition to the raw
// points supported by elliptic.Unmarshal. It assumes a NIST curve, and
// specifically that a = -3. It's faster when p = 3 mod 4 because of how
// ModSqrt works.
func (p *Point) Unmarshal(curve elliptic.Curve, data []byte) error {
	if curve == nil {
		return ErrUnspecifiedCurve
	}
	byteLen := (curve.Params().BitSize + 7) >> 3
	fieldOrder := curve.Params().P
	if len(data) == byteLen+1 {
		// Compressed point
		x := new(big.Int).SetBytes(data[1 : 1+byteLen])
		if x.Cmp(fieldOrder) != -1 {
			// x in [0, p-1]
			return ErrInvalidPoint
		}
		if data[0] == 0x02 || data[0] == 0x03 {
			sign := data[0] & 1 // "mod 2"

			// Recall y² = x³ - 3x + b
			// obviously, the Lsh trick is only valid when a = -3
			x3 := new(big.Int).Mul(x, x)          // x^2
			x3.Mul(x3, x)                         // x(x^2)
			threeTimesX := new(big.Int).Lsh(x, 1) // x << 1 == x*2
			threeTimesX.Add(threeTimesX, x)       // (x << 1) + x == x*3
			x3.Sub(x3, threeTimesX)               // x^3 - 3x
			x3.Add(x3, curve.Params().B)          // x^3 - 3x + b
			y := x3.ModSqrt(x3, fieldOrder)       // sqrt(x^3 - 3x + b) (mod p)
			if y == nil {
				// if no square root exists, either marshaling error
				// or an invalid curve point
				return ErrInvalidPoint
			}
			if sign != isOdd(y) {
				y.Sub(fieldOrder, y)
			}
			if !curve.IsOnCurve(x, y) {
				x = nil
				y = nil
				return ErrInvalidPoint
			}
			p.Curve = curve
			p.X, p.Y = x, y
			return nil
		} else {
			return ErrInvalidPoint
		}
	}
	if len(data) == (2*byteLen)+1 && data[0] == 0x04 {
		// Uncompressed point
		p.Curve = curve
		p.X, p.Y = elliptic.Unmarshal(curve, data)
		if p.X == nil {
			return ErrInvalidPoint
		}
		return nil
	}
	return ErrInvalidPoint
}

func isOdd(x *big.Int) byte {
	return byte(x.Bit(0) & 1)
}

// BatchUnmarshalPoints takes a slice of P-256 curve points in the form specified
// in section 4.3.6 of ANSI X9.62 (see Go crypto/elliptic) and retuns a slice
// of crypto.Point instances.
func BatchUnmarshalPoints(curve elliptic.Curve, data [][]byte) ([]*Point, error) {
	if curve == nil {
		return nil, ErrUnspecifiedCurve
	}
	decoded := make([]*Point, len(data))
	for i := 0; i < len(data); i++ {
		p := &Point{Curve: curve, X: nil, Y: nil}
		err := p.Unmarshal(curve, data[i])
		if err != nil {
			return nil, err
		}
		decoded[i] = p
	}
	return decoded, nil
}

// BatchMarshalPoints encodes a slice of crypto.Point objects in the form
// specified in section 4.3.6 of ANSI X9.62.
func BatchMarshalPoints(points []*Point) ([][]byte, error) {
	data := make([][]byte, len(points))
	for i := 0; i < len(points); i++ {
		data[i] = points[i].Marshal()
	}
	return data, nil
}

// We combine the marshaled points with the DLEQ proof
// This function splits the points and the DLEQ proof for further unmarshaling
func GetMarshaledPointsAndDleq(data [][]byte) ([][]byte, []byte) {
	marshaledPoints := make([][]byte, len(data)-1)
	var batchProof []byte
	for i, v := range data {
		if i < len(data)-1 {
			marshaledPoints[i] = v
		} else {
			batchProof = data[i]
		}
	}

	return marshaledPoints, batchProof
}

func NewPoint(curve elliptic.Curve, x, y *big.Int) (*Point, error) {
	if curve == nil {
		return nil, ErrUnspecifiedCurve
	}
	if !curve.IsOnCurve(x, y) {
		return nil, ErrPointOffCurve
	}
	return &Point{Curve: curve, X: x, Y: y}, nil
}

func NewRandomPoint(curve elliptic.Curve) (seed []byte, P *Point, ret error) {
	if curve == nil {
		return nil, nil, ErrUnspecifiedCurve
	}
	byteLen := (curve.Params().BitSize + 7) >> 3
	seed = make([]byte, byteLen)
	_, err := io.ReadFull(rand.Reader, seed)
	if err != nil {
		return nil, nil, err
	}
	var hash crypto.Hash
	switch curve {
	case elliptic.P256():
		hash = crypto.SHA256
	case elliptic.P384():
		hash = crypto.SHA384
	case elliptic.P521():
		hash = crypto.SHA512
	default:
		hash = crypto.SHA256
	}
	P, ret = HashToCurve(curve, hash, seed)
	return seed, P, ret
}

// This function hashes data to a point on the specified curve without
// revealing the discrete logarithm w.r.t. a generator. It is not constant time
// and thus potentially leaks information about the input data. The operation
// works by repeatedly hashing the data, tagging it as a SEC1 compressed curve
// point, and checking if the decompression produces a valid point.
func HashToCurve(curve elliptic.Curve, hash crypto.Hash, data []byte) (*Point, error) {
	if curve == nil {
		return nil, ErrUnspecifiedCurve
	}
	byteLen := (curve.Params().BitSize + 7) >> 3

	var separator string
	switch curve {
	case elliptic.P256():
		separator = "1.2.840.10045.3.1.7 point generation seed"
	case elliptic.P384():
		separator = "1.3.132.0.34 point generation seed"
	case elliptic.P521():
		separator = "1.3.132.0.35 point generation seed"
	default:
		// Decompression assumes a = -3 so this could fail.
		separator = "domain separator for point hashing"
	}

	var P = &Point{Curve: curve, X: nil, Y: nil}
	var buf = make([]byte, byteLen+1)
	var ctr = make([]byte, 4)
	var h = hash.New()
	h.Write([]byte(separator))
	for i := 0; i < 10; i++ {
		binary.LittleEndian.PutUint32(ctr, uint32(i))
		h.Write(data)
		h.Write(ctr)
		sum := h.Sum(nil)
		copy(buf[1:1+byteLen], sum[:byteLen])

		buf[0] = 0x02
		err := P.Unmarshal(curve, buf)
		if err == nil {
			return P, nil
		}
		buf[0] = 0x03
		err = P.Unmarshal(curve, buf)
		if err == nil {
			return P, nil
		}

		data = sum
		h.Reset()
	}
	return nil, ErrNoPointFound
}

// This is just a bitmask with the number of ones starting at 8 then
// incrementing by index. To account for fields with bitsizes that are not a whole
// number of bytes, we mask off the unnecessary bits. h/t agl
var mask = []byte{0xff, 0x1, 0x3, 0x7, 0xf, 0x1f, 0x3f, 0x7f}

func randScalar(curve elliptic.Curve, rand io.Reader) ([]byte, *big.Int, error) {
	N := curve.Params().N // base point subgroup order
	bitLen := N.BitLen()
	byteLen := (bitLen + 7) >> 3
	buf := make([]byte, byteLen)

	for true {
		_, err := io.ReadFull(rand, buf)
		if err != nil {
			return nil, nil, err
		}
		// Mask to account for field sizes that are not a whole number of bytes.
		buf[0] &= mask[bitLen%8]
		// Check if scalar is in the correct range.
		if new(big.Int).SetBytes(buf).Cmp(N) >= 0 {
			continue
		}
		break
	}

	return buf, new(big.Int).SetBytes(buf), nil
}

// We want to be able to load commitments in from file as part
// of enabling DLEQ proof batching. Perform this sanity check to make
// sure that commitments work properly.
// Also returns point representations
func RetrieveCommPoints(GBytes, HBytes, key []byte) (*Point, *Point, error) {
	G := &Point{Curve: elliptic.P256(), X: nil, Y: nil}
	err := G.Unmarshal(G.Curve, GBytes)
	if err != nil {
		return nil, nil, err
	}
	H := &Point{Curve: elliptic.P256(), X: nil, Y: nil}
	err = H.Unmarshal(H.Curve, HBytes)
	if err != nil {
		return nil, nil, err
	}
	curve := elliptic.P256()
	chkHX, chkHY := curve.ScalarMult(G.X, G.Y, key)
	chkH := &Point{Curve: elliptic.P256(), X: chkHX, Y: chkHY}
	hash := crypto.SHA256
	chkHash := hash.New()
	_, err = chkHash.Write(chkH.Marshal())
	if err != nil {
		return nil, nil, err
	}

	h := hash.New()
	_, err = h.Write(H.Marshal())
	if err != nil {
		return nil, nil, err
	}

	if !bytes.Equal(h.Sum(nil), chkHash.Sum(nil)) {
		return nil, nil, ErrCommSanityCheck
	}

	return G, H, nil
}
