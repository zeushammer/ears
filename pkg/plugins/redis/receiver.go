// Copyright 2020 Comcast Cable Communications Management, LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-redis/redis"
	"github.com/rs/zerolog"
	"os"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/xmidt-org/ears/pkg/event"
	pkgplugin "github.com/xmidt-org/ears/pkg/plugin"
	"github.com/xmidt-org/ears/pkg/receiver"
)

func NewReceiver(config interface{}) (receiver.Receiver, error) {
	var cfg ReceiverConfig
	var err error
	switch c := config.(type) {
	case string:
		err = yaml.Unmarshal([]byte(c), &cfg)
	case []byte:
		err = yaml.Unmarshal(c, &cfg)
	case ReceiverConfig:
		cfg = c
	case *ReceiverConfig:
		cfg = *c
	}
	if err != nil {
		return nil, &pkgplugin.InvalidConfigError{
			Err: err,
		}
	}
	cfg = cfg.WithDefaults()
	err = cfg.Validate()
	if err != nil {
		return nil, err
	}
	logger := zerolog.New(os.Stdout).Level(zerolog.DebugLevel)
	//zerolog.LevelFieldName = "log.level"
	r := &Receiver{
		config: cfg,
		logger: logger,
	}
	return r, nil
}

func (r *Receiver) Receive(next receiver.NextFn) error {
	if r == nil {
		return &pkgplugin.Error{
			Err: fmt.Errorf("Receive called on <nil> pointer"),
		}
	}
	if next == nil {
		return &receiver.InvalidConfigError{
			Err: fmt.Errorf("next cannot be nil"),
		}
	}
	r.Lock()
	r.startTime = time.Now()
	r.next = next
	r.done = make(chan struct{})
	r.stopped = false
	r.Unlock()
	go func() {
		defer func() {
			r.Lock()
			if !r.stopped {
				r.done <- struct{}{}
			}
			r.Unlock()
		}()
		r.redisClient = redis.NewClient(&redis.Options{
			Addr:     r.config.Endpoint,
			Password: "",
			DB:       0,
		})
		defer r.redisClient.Close()
		r.pubsub = r.redisClient.Subscribe(r.config.Channel)
		defer r.pubsub.Close()
		for {
			// could have a pool of go routines consuming from this channel here
			msg := <-r.pubsub.Channel()
			if r.stopped {
				r.logger.Info().Str("op", "redis.Receive").Msg("stopping receive loop")
				return
			}
			r.logger.Info().Str("op", "redis.Receive").Msg("received message on redis channel")
			var pl interface{}
			err := json.Unmarshal([]byte(msg.Payload), &pl)
			if err != nil {
				r.logger.Error().Str("op", "redis.Receive").Msg("cannot parse payload: " + err.Error())
				continue
			}
			r.Lock()
			r.count++
			r.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(5)*time.Second)
			// note: if we just pass msg.Payload into event, redis will blow up with an out of memory error within a
			// few seconds - possibly a bug in the client library
			e, err := event.New(ctx, pl, event.WithAck(
				func(e event.Event) {
					r.logger.Info().Str("op", "redis.Receive").Msg("processed message from redis channel")
					cancel()
				},
				func(e event.Event, err error) {
					r.logger.Error().Str("op", "redis.Receive").Msg("failed to process message: " + err.Error())
					cancel()
				}))
			if err != nil {
				r.logger.Error().Str("op", "redis.Receive").Msg("cannot create event: " + err.Error())
				continue
			}
			r.Trigger(e)
		}
	}()
	r.logger.Info().Str("op", "redis.Receive").Msg("waiting for receive done")
	<-r.done
	r.Lock()
	elapsedMs := time.Since(r.startTime).Milliseconds()
	throughput := 1000 * r.count / (int(elapsedMs) + 1)
	cnt := r.count
	r.Unlock()
	r.logger.Info().Str("op", "redis.Receive").Int("elapsedMs", int(elapsedMs)).Int("count", cnt).Int("throughput", throughput).Msg("receive done")
	return nil
}

func (r *Receiver) Count() int {
	r.Lock()
	defer r.Unlock()
	return r.count
}

func (r *Receiver) StopReceiving(ctx context.Context) error {
	r.Lock()
	if !r.stopped {
		r.stopped = true
		r.pubsub.Unsubscribe(r.config.Channel)
		r.pubsub.Close()
		close(r.done)
	}
	r.Unlock()
	return nil
}

func (r *Receiver) Trigger(e event.Event) {
	r.Lock()
	next := r.next
	r.Unlock()
	next(e)
}

func (r *Receiver) Config() interface{} {
	return r.config
}

func (r *Receiver) Name() string {
	return ""
}

func (r *Receiver) Plugin() string {
	return "redis"
}