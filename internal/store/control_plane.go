package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// RegisterControlPlaneInstance records live mixed-version membership and
// rejects protocol intervals that cannot safely overlap. It does not elect a
// leader; durable operations continue to use independently fenced leases.
func (s *Store) RegisterControlPlaneInstance(ctx context.Context, instance domain.ControlPlaneInstance, liveFor time.Duration) error {
	if instance.ID == "" || instance.BinaryVersion == "" || instance.ProtocolMin < 1 || instance.ProtocolMax < instance.ProtocolMin || liveFor <= 0 {
		return errors.New("instance identity, binary version, valid protocol interval, and positive live interval are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := time.Now().UTC().Add(-liveFor)
	rows, err := tx.QueryContext(ctx, `SELECT instance_id,binary_version,protocol_min,protocol_max FROM control_plane_instances WHERE heartbeat_at>=? AND instance_id<>? FOR SHARE`, cutoff, instance.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, binary string
		var minimum, maximum int
		if err = rows.Scan(&id, &binary, &minimum, &maximum); err != nil {
			rows.Close()
			return err
		}
		if instance.ProtocolMax < minimum || maximum < instance.ProtocolMin {
			rows.Close()
			return fmt.Errorf("control-plane protocol %d..%d is incompatible with live instance %s (%s, protocol %d..%d)", instance.ProtocolMin, instance.ProtocolMax, id, binary, minimum, maximum)
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	stamp := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO control_plane_instances(instance_id,binary_version,protocol_min,protocol_max,started_at,heartbeat_at,draining) VALUES(?,?,?,?,?,?,FALSE) ON CONFLICT(instance_id) DO UPDATE SET binary_version=EXCLUDED.binary_version,protocol_min=EXCLUDED.protocol_min,protocol_max=EXCLUDED.protocol_max,started_at=EXCLUDED.started_at,heartbeat_at=EXCLUDED.heartbeat_at,draining=FALSE`, instance.ID, instance.BinaryVersion, instance.ProtocolMin, instance.ProtocolMax, stamp, stamp)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) HeartbeatControlPlaneInstance(ctx context.Context, id string) error {
	result, err := s.ExecContext(ctx, `UPDATE control_plane_instances SET heartbeat_at=?,draining=FALSE WHERE instance_id=?`, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) UnregisterControlPlaneInstance(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("instance identity is required")
	}
	_, err := s.ExecContext(ctx, `DELETE FROM control_plane_instances WHERE instance_id=?`, id)
	return err
}

func (s *Store) ControlPlaneInstances(ctx context.Context, liveFor time.Duration) ([]domain.ControlPlaneInstance, error) {
	if liveFor <= 0 {
		return nil, errors.New("positive live interval is required")
	}
	rows, err := s.QueryContext(ctx, `SELECT instance_id,binary_version,protocol_min,protocol_max,started_at,heartbeat_at,draining FROM control_plane_instances WHERE heartbeat_at>=? ORDER BY instance_id`, time.Now().UTC().Add(-liveFor))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ControlPlaneInstance
	for rows.Next() {
		var item domain.ControlPlaneInstance
		if err = rows.Scan(&item.ID, &item.BinaryVersion, &item.ProtocolMin, &item.ProtocolMax, &item.StartedAt, &item.HeartbeatAt, &item.Draining); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
