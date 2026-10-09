package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	tether "github.com/hollis-labs/go-tether-client"
)

// RecordRefusal retains a bounded diagnostic identity without claiming to
// archive an inadmissible body. The committed cursor remains unchanged and
// history must re-fetch this publication; only a genuine purge can settle it.
func (l *Ledger) RecordRefusal(ctx context.Context, source Source, message tether.ChannelMessage) error {
	digest := sha256.Sum256(message.Payload)
	id := message.ID
	if len(id) > 256 {
		id = "sha256:" + payloadDigest([]byte(id))
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO admission_refusals(source,message_id,sequence,payload_digest,payload_bytes) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, source.key(), id, message.Seq, hex.EncodeToString(digest[:]), len(message.Payload))
	return err
}

func payloadDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
