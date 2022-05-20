package nrpc

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/tehsphinx/nrpc/pubsub"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func newClientStream(pub pubsub.Publisher, sub pubsub.Subscriber, log Logger, method string, opts []grpc.CallOption) *clientStream {
	randSuffix := randString(randSubjectLen)
	s := &clientStream{
		pub:        pub,
		sub:        sub,
		log:        log,
		method:     method,
		methodSubj: methodSubj(method),
		reqSubj:    "nrpc.req" + strings.ReplaceAll(method, "/", ".") + "." + randSuffix,
		respSubj:   "nrpc.resp" + strings.ReplaceAll(method, "/", ".") + "." + randSuffix,
		opts:       opts,
		chRecv:     make(chan *respMsg, 1),
	}
	return s
}

type clientStream struct {
	pub pubsub.Publisher
	sub pubsub.Subscriber
	log Logger

	ctx        context.Context
	cancel     context.CancelFunc
	method     string
	methodSubj string
	reqSubj    string
	respSubj   string
	opts       []grpc.CallOption

	firstSent   bool
	sendClosed  bool
	chRecv      chan *respMsg
	recvHeader  metadata.MD
	recvTrailer metadata.MD
}

// Header returns the header metadata received from the server if there
// is any. It blocks if the metadata is not ready to read.
func (s *clientStream) Header() (metadata.MD, error) {
	return s.recvHeader, nil
}

// Trailer returns the trailer metadata from the server, if there is any.
// It must only be called after stream.CloseAndRecv has returned, or
// stream.Recv has returned a non-nil error (including io.EOF).
func (s *clientStream) Trailer() metadata.MD {
	return s.recvTrailer
}

// CloseSend closes the send direction of the stream. It closes the stream
// when non-nil error is met. It is also not safe to call CloseSend
// concurrently with SendMsg.
func (s *clientStream) CloseSend() error {
	payload, err := marshalEOS()
	if err != nil {
		return err
	}
	s.sendClosed = true

	return s.pub.Publish(pubsub.Message{
		Subject: s.reqSubj,
		Data:    payload,
	})
}

// Context returns the context for this stream.
//
// It should not be called until after Header or RecvMsg has returned. Once
// called, subsequent client-side retries are disabled.
func (s *clientStream) Context() context.Context {
	return s.ctx
}

// SendMsg is generally called by generated code. On error, SendMsg aborts
// the stream. If the error was generated by the client, the status is
// returned directly; otherwise, io.EOF is returned and the status of
// the stream may be discovered using RecvMsg.
//
// SendMsg blocks until:
//   - There is sufficient flow control to schedule m with the transport, or
//   - The stream is done, or
//   - The stream breaks.
//
// SendMsg does not wait until the message is received by the server. An
// untimely stream closure may result in lost messages. To ensure delivery,
// users should ensure the RPC completed successfully using RecvMsg.
//
// It is safe to have a goroutine calling SendMsg and another goroutine
// calling RecvMsg on the same stream at the same time, but it is not safe
// to call SendMsg on the same stream in different goroutines. It is also
// not safe to call CloseSend concurrently with SendMsg.
func (s *clientStream) SendMsg(m interface{}) error {
	if s.sendClosed {
		return io.EOF
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}
	// nolint: forcetypeassert
	args := m.(proto.Message)

	subj, reqSubj, respSubj := s.getSubjects()
	payload, err := marshalReqMsg(s.ctx, args, reqSubj, respSubj, 0)
	if err != nil {
		return err
	}

	return s.sendMsg(subj, payload)
}

func (s *clientStream) getSubjects() (string, string, string) {
	if s.firstSent {
		return s.reqSubj, "", ""
	}
	return s.methodSubj, s.reqSubj, s.respSubj
}

func (s *clientStream) sendMsg(subj string, payload []byte) error {
	if s.firstSent {
		return s.pub.Publish(pubsub.Message{
			Subject: subj,
			Data:    payload,
		})
	}

	ctx, cancel := context.WithTimeout(s.ctx, streamConnectTimeout)
	defer cancel()

	resp, err := s.pub.Request(ctx, pubsub.Message{
		Subject: subj,
		Data:    payload,
	})
	if err != nil {
		return err
	}
	if len(resp.Data) != 0 {
		return errors.New("unexpected response")
	}
	s.firstSent = true

	return nil
}

// RecvMsg blocks until it receives a message into m or the stream is
// done. It returns io.EOF when the stream completes successfully. On
// any other error, the stream is aborted and the error contains the RPC
// status.
//
// It is safe to have a goroutine calling SendMsg and another goroutine
// calling RecvMsg on the same stream at the same time, but it is not
// safe to call RecvMsg on the same stream in different goroutines.
func (s *clientStream) RecvMsg(target interface{}) error {
	for {
		resp, err := s.recvMsg(target)
		if err != nil {
			return err
		}
		if resp.HeaderOnly {
			continue
		}
		return nil
	}
}

func (s *clientStream) recvMsg(target interface{}) (*Response, error) {
	var recv *respMsg
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case recv = <-s.chRecv:
	}

	resp, err := unmarshalRespMsg(recv.data, target)
	if err != nil {
		return nil, err
	}
	if resp.Eos {
		s.cancel()
		if resp.Data != nil {
			return nil, unmarshalErr(resp.Data)
		}
		return nil, io.EOF
	}
	if resp.Header != nil {
		s.recvHeader = toMD(resp.Header)
	}
	if resp.Trailer != nil {
		s.recvTrailer = toMD(resp.Trailer)
	}
	return resp, nil
}

// Subscribe subscribes to the server stream.
func (s *clientStream) Subscribe(ctx context.Context) error {
	queue := "receive"

	s.ctx, s.cancel = context.WithCancel(ctx)

	s.log.Infof("Subscribed Stream (client): Subject => %s, Queue => %s", s.respSubj, queue)
	sub, err := s.sub.Subscribe(s.respSubj, queue, func(ctx context.Context, msg pubsub.Replier) {
		// dbg.Cyan("server -> client (received)", msg.Subject(), msg.Data())
		select {
		case <-s.ctx.Done():
			return
		case s.chRecv <- &respMsg{ctx: ctx, data: msg.Data()}:
		default:
			select {
			case <-s.ctx.Done():
				return
			case <-ctx.Done():
				s.cancel()
				return
			case s.chRecv <- &respMsg{ctx: ctx, data: msg.Data()}:
			case <-time.After(stuckTimeout):
				s.log.Errorf("Stream: Subject => %s, Queue => %s: closing stream: "+
					"client stream consumer stuck for 30sec", s.respSubj, queue)
				s.cancel()
			}
		}
	})
	if err != nil {
		return err
	}
	go func() {
		<-s.ctx.Done()
		_ = sub.Unsubscribe()
	}()

	return err
}
