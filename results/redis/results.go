package redis

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	resultPrefix = "tq:res:"

	// Suffix for hashmaps storing success/failed job ids
	success = "success"
	failed  = "failed"
)

type Results struct {
	opts Options
	lo   *slog.Logger
	conn redis.UniversalClient
	pipe redis.Pipeliner
}

type Options struct {
	Addrs        []string
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	Expiry       time.Duration
	MetaExpiry   time.Duration
	MinIdleConns int

	// OPTIONAL
	// If non-zero, enqueue redis commands will be piped instead of being directly sent each time.
	// The pipe will be executed every `PipePeriod` duration.
	PipePeriod time.Duration
}

func DefaultRedis() Options {
	return Options{
		Addrs:    []string{"127.0.0.1:6379"},
		Password: "",
		DB:       0,
	}
}

func New(o Options, lo *slog.Logger) *Results {
	rs := &Results{
		opts: o,
		conn: redis.NewUniversalClient(
			&redis.UniversalOptions{
				Addrs:           o.Addrs,
				Password:        o.Password,
				DB:              o.DB,
				DialTimeout:     o.DialTimeout,
				ReadTimeout:     o.ReadTimeout,
				WriteTimeout:    o.WriteTimeout,
				ConnMaxIdleTime: o.IdleTimeout,
				MinIdleConns:    o.MinIdleConns,
			},
		),
		lo: lo,
	}

	// TODO: pass ctx here somehow
	if o.MetaExpiry != 0 {
		go rs.expireMeta(o.MetaExpiry)
	}
	if o.PipePeriod != 0 {
		rs.pipe = rs.conn.Pipeline()
		go rs.execPipe(context.TODO())
	}

	return rs
}

func (r *Results) execPipe(ctx context.Context) {
	tk := time.NewTicker(r.opts.PipePeriod)
	for {
		select {
		case <-ctx.Done():
			r.lo.Debug("context closed, draining redis pipe", "length", r.pipe.Len())
			if _, err := r.pipe.Exec(ctx); err != nil {
				r.lo.Error("could not execute redis pipe", "error", err)
			}
			return
		case <-tk.C:
			plen := r.pipe.Len()
			if plen == 0 {
				continue
			}
			r.lo.Debug("submitting redis pipe", "length", plen)
			if _, err := r.pipe.Exec(ctx); err != nil {
				r.lo.Error("could not execute redis pipe", "error", err)
			}
		}
	}
}

func (r *Results) DeleteJob(ctx context.Context, id string) error {
	r.lo.Debug("deleting job")

	pipe := r.conn.Pipeline()
	if err := pipe.ZRem(ctx, resultPrefix+success, 1, id).Err(); err != nil {
		return err
	}
	if err := pipe.ZRem(ctx, resultPrefix+failed, 1, id).Err(); err != nil {
		return err
	}
	if err := pipe.Del(ctx, resultPrefix+id).Err(); err != nil {
		return err
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}

	return nil
}

func (r *Results) GetSuccess(ctx context.Context) ([]string, error) {
	// Fetch the failed tasks with score less than current time
	r.lo.Debug("getting successful jobs")
	rs, err := r.conn.ZRevRangeByScore(ctx, resultPrefix+success, &redis.ZRangeBy{
		Min: "0",
		Max: strconv.FormatInt(time.Now().UnixNano(), 10),
	}).Result()
	if err != nil {
		return nil, err
	}

	return rs, nil
}

func (r *Results) GetFailed(ctx context.Context) ([]string, error) {
	// Fetch the failed tasks with score less than current time
	r.lo.Debug("getting failed jobs")
	rs, err := r.conn.ZRevRangeByScore(ctx, resultPrefix+failed, &redis.ZRangeBy{
		Min: "0",
		Max: strconv.FormatInt(time.Now().UnixNano(), 10),
	}).Result()
	if err != nil {
		return nil, err
	}

	return rs, nil
}

func (r *Results) SetSuccess(ctx context.Context, id string) error {
	r.lo.Debug("setting job as successful", "id", id)
	if r.opts.PipePeriod != 0 {
		return r.pipe.ZAdd(ctx, resultPrefix+success, redis.Z{
			Score:  float64(time.Now().UnixNano()),
			Member: id,
		}).Err()
	}
	return r.conn.ZAdd(ctx, resultPrefix+success, redis.Z{
		Score:  float64(time.Now().UnixNano()),
		Member: id,
	}).Err()
}

func (r *Results) SetFailed(ctx context.Context, id string) error {
	r.lo.Debug("setting job as failed", "id", id)
	if r.opts.PipePeriod != 0 {
		return r.pipe.ZAdd(ctx, resultPrefix+failed, redis.Z{
			Score:  float64(time.Now().UnixNano()),
			Member: id,
		}).Err()
	}
	return r.conn.ZAdd(ctx, resultPrefix+failed, redis.Z{
		Score:  float64(time.Now().UnixNano()),
		Member: id,
	}).Err()
}

func (r *Results) Set(ctx context.Context, id string, b []byte) error {
	r.lo.Debug("setting result for job", "id", id)
	if r.opts.PipePeriod != 0 {
		return r.pipe.Set(ctx, resultPrefix+id, b, r.opts.Expiry).Err()
	}
	return r.conn.Set(ctx, resultPrefix+id, b, r.opts.Expiry).Err()
}

func (r *Results) Get(ctx context.Context, id string) ([]byte, error) {
	r.lo.Debug("getting result for job", "id", id)
	rs, err := r.conn.Get(ctx, resultPrefix+id).Bytes()
	if err != nil {
		return nil, err
	}

	return rs, nil
}

// TODO: accpet a ctx here and shutdown gracefully
func (r *Results) expireMeta(ttl time.Duration) {
	r.lo.Info("starting results meta purger", "ttl", ttl)

	var (
		tk = time.NewTicker(ttl)
	)

	for {
		select {
		// case <-ctx.Done():
		// 	r.lo.Info("shutting down meta purger", "ttl", ttl)
		// 	return
		case <-tk.C:
			now := time.Now().UnixNano() - int64(ttl)
			score := strconv.FormatInt(now, 10)

			r.lo.Debug("purging failed results metadata", "score", score)
			if r.opts.PipePeriod != 0 {
				if err := r.pipe.ZRemRangeByScore(context.Background(), resultPrefix+failed, "0", score).Err(); err != nil {
					r.lo.Error("could not expire success/failed metadata", "err", err)
				}
				r.lo.Debug("purging success results metadata", "score", score)
				if err := r.pipe.ZRemRangeByScore(context.Background(), resultPrefix+success, "0", score).Err(); err != nil {
					r.lo.Error("could not expire success/failed metadata", "err", err)
				}
			} else {
				if err := r.conn.ZRemRangeByScore(context.Background(), resultPrefix+failed, "0", score).Err(); err != nil {
					r.lo.Error("could not expire success/failed metadata", "err", err)
				}
				r.lo.Debug("purging success results metadata", "score", score)
				if err := r.conn.ZRemRangeByScore(context.Background(), resultPrefix+success, "0", score).Err(); err != nil {
					r.lo.Error("could not expire success/failed metadata", "err", err)
				}
			}
		}
	}
}

func (r *Results) NilError() error {
	return redis.Nil
}
