/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package streaming

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	streamapi "github.com/containerd/containerd/api/services/streaming/v1"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/containerd/nerdbox/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "streaming",
		Requires: []plugin.Type{
			plugins.SandboxPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			sb, err := ic.GetSingle(plugins.SandboxPlugin)
			if err != nil {
				return nil, err
			}

			return &service{
				ss: sb.(sandboxStreamer),
			}, nil
		},
	})
}

type sandboxStreamer interface {
	StartStream(context.Context, string) (net.Conn, error)
}

type service struct {
	ss sandboxStreamer
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	streamapi.RegisterTTRPCStreamingService(server, s)
	return nil
}

func (s *service) Stream(ctx context.Context, srv streamapi.TTRPCStreaming_StreamServer) error {
	// Receive the StreamInit message with the stream ID
	a, err := srv.Recv()
	if err != nil {
		return err
	}
	var i streamapi.StreamInit
	if err := typeurl.UnmarshalTo(a, &i); err != nil {
		return err
	}

	log.G(ctx).WithField("stream", i.ID).Debug("creating stream bridge")

	// Create a stream connection to the sandbox, passing through the stream ID
	stream, err := s.ss.StartStream(ctx, i.ID)
	if err != nil {
		return fmt.Errorf("failed to start sandbox stream: %w", err)
	}
	defer stream.Close()

	log.G(ctx).WithField("stream", i.ID).Debug("stream bridge established")

	// Send ack back to containerd client
	e, _ := typeurl.MarshalAnyToProto(&ptypes.Empty{})
	if err := srv.Send(e); err != nil {
		return err
	}

	var (
		sendToStreamC   = bridgeTTRPCToStream(srv, stream)
		recvFromStreamC = bridgeStreamToTTRPC(stream, srv)
	)

	// Wait for both bridge directions to finish or context cancellation.
	// When the host->sandbox direction finishes first, half-close the
	// write side so the sandbox sees EOF, then wait for the sandbox to
	// finish sending its response. When the sandbox->host direction
	// finishes first, return immediately — closing the TTRPC stream
	// signals EOF to the client, and defer stream.Close() cleans up.
	select {
	case err := <-sendToStreamC:
		if err != nil {
			log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("host->sandbox bridge ended")
		}
		// Half-close the write side so the sandbox sees EOF on its
		// reads while still allowing data to flow back.
		if cw, ok := stream.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		// Wait for the sandbox to finish sending.
		select {
		case err := <-recvFromStreamC:
			if err != nil {
				log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("sandbox->host bridge ended")
			}
		case <-ctx.Done():
		}
	case err := <-recvFromStreamC:
		if err != nil {
			log.G(ctx).WithError(err).WithField("stream", i.ID).Debug("sandbox->host bridge ended")
		}
	case <-ctx.Done():
	}

	return nil
}

// bridgeTTRPCToStream reads typeurl.Any messages from the TTRPC stream and
// writes them as length-prefixed proto frames to the stream connection.
func bridgeTTRPCToStream(srv streamapi.TTRPCStreaming_StreamServer, conn io.Writer) chan error {
	errC := make(chan error, 1)
	go func() {
		defer close(errC)
		var (
			err  error
			a    *anypb.Any
			data []byte
		)
		defer func() {
			if err != nil {
				errC <- err
			}
		}()

		for {
			a, err = srv.Recv()
			if err != nil {
				return
			}

			data, err = proto.Marshal(typeurl.MarshalProto(a))
			if err != nil {
				err = fmt.Errorf("failed to marshal for sandbox stream: %w", err)
				return
			}
			if err = binary.Write(conn, binary.BigEndian, uint32(len(data))); err != nil {
				err = fmt.Errorf("failed to write frame length to sandbox stream: %w", err)
				return
			}
			if _, err = conn.Write(data); err != nil {
				err = fmt.Errorf("failed to write frame data to sandbox stream: %w", err)
				return
			}
		}
	}()
	return errC
}

// bridgeStreamToTTRPC reads length-prefixed proto frames from the stream
// connection and sends them as typeurl.Any messages on the TTRPC stream.
func bridgeStreamToTTRPC(conn io.Reader, srv streamapi.TTRPCStreaming_StreamServer) chan error {
	errC := make(chan error, 1)
	go func() {
		defer close(errC)
		var (
			err  error
			a    anypb.Any
			data []byte
		)
		defer func() {
			if err != nil {
				errC <- err
			}
		}()

		for {
			var length uint32
			if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
				return
			}
			if length == 0 {
				continue
			}
			if length > uint32(len(data)) {
				// Resize data buffer if incoming frame is larger than current buffer
				data = make([]byte, length)
			}
			buf := data[:length]
			if _, err = io.ReadFull(conn, buf); err != nil {
				err = fmt.Errorf("failed to read frame data from sandbox stream: %w", err)
				return
			}
			if err = proto.Unmarshal(buf, &a); err != nil {
				err = fmt.Errorf("failed to unmarshal from sandbox stream: %w", err)
				return
			}
			if err = srv.Send(&a); err != nil {
				return
			}
		}
	}()
	return errC
}
