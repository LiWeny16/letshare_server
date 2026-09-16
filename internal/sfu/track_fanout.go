package sfu

import (
	"sync"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

// trackFanout owns the single ReadRTP loop for one publisher track. A
// TrackRemote must not be read concurrently by one goroutine per subscriber:
// concurrent readers compete for packets and make video delivery randomly
// lose keyframes. The fanout reads once and writes each packet to every
// subscriber's local delivery track.
type trackFanout struct {
	remote  *webrtc.TrackRemote
	mu      sync.RWMutex
	outputs map[string]*webrtc.TrackLocalStaticRTP
	stop    chan struct{}
	once    sync.Once
}

func newTrackFanout(remote *webrtc.TrackRemote) *trackFanout {
	fanout := &trackFanout{
		remote:  remote,
		outputs: make(map[string]*webrtc.TrackLocalStaticRTP),
		stop:    make(chan struct{}),
	}
	go fanout.run()
	return fanout
}

func (f *trackFanout) add(key string, local *webrtc.TrackLocalStaticRTP) {
	if local == nil || key == "" {
		return
	}
	f.mu.Lock()
	f.outputs[key] = local
	f.mu.Unlock()
}

func (f *trackFanout) remove(key string) {
	if key == "" {
		return
	}
	f.mu.Lock()
	delete(f.outputs, key)
	f.mu.Unlock()
}

func (f *trackFanout) close() {
	f.once.Do(func() {
		close(f.stop)
		_ = f.remote.SetReadDeadline(noReadDeadline())
	})
}

func (f *trackFanout) run() {
	for {
		select {
		case <-f.stop:
			return
		default:
		}
		packet, _, err := f.remote.ReadRTP()
		if err != nil {
			select {
			case <-f.stop:
			default:
				log.WithError(err).WithField("trackID", f.remote.ID()).Debug("发布轨道读取结束")
			}
			return
		}
		f.mu.RLock()
		outputs := make([]*webrtc.TrackLocalStaticRTP, 0, len(f.outputs))
		for _, local := range f.outputs {
			outputs = append(outputs, local)
		}
		f.mu.RUnlock()
		for _, local := range outputs {
			_ = local.WriteRTP(packet)
		}
	}
}
