package seccall

import (
	"context"
	"database/sql/driver"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// notifyChannel is the Postgres channel the relay announces new messages on
// (payload: the room id). Every instance LISTENs on it and wakes its own long
// polls for that room.
const notifyChannel = "seccall_relay"

// StartListener keeps one dedicated connection LISTENing for relay
// notifications until ctx ends. While it is down, long polls re-check the
// database every second instead of every few seconds.
func (s *Service) StartListener(ctx context.Context) {
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			err := s.listenOnce(ctx)
			s.listening.Store(false)
			if ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, errNoPgx) {
				log.Printf("seccall: relay listener: %v (retrying in %s)", err, backoff)
			}
			if errors.Is(err, errNoPgx) {
				log.Printf("seccall: relay listener unavailable (not a pgx connection); long polls re-check every second")
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
}

var errNoPgx = errors.New("not a pgx connection")

func (s *Service) listenOnce(ctx context.Context) error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		sc, ok := dc.(*stdlib.Conn)
		if !ok {
			return errNoPgx
		}
		pc := sc.Conn()
		if _, err := pc.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
			return err
		}
		s.listening.Store(true)
		for {
			n, err := pc.WaitForNotification(ctx)
			if err != nil {
				// the connection is unusable after a failed wait: let
				// database/sql discard it
				return errors.Join(err, driver.ErrBadConn)
			}
			s.hub.wake(n.Payload)
		}
	})
}

// listenerUp reports whether this instance currently receives notifications.
func (s *Service) listenerUp() bool { return s.listening.Load() }
