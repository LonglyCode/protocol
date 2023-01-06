package ingress

import (
	"context"
	"errors"
	"time"

	"github.com/go-redis/redis/v8"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/service/rpc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
)

const (
	updateChannel         = "IN_RESULTS"
	entityChannel         = "IN_ENTITY"
	requestChannelPrefix  = "REQ_"
	responseChannelPrefix = "RES_"

	RequestExpiration = time.Second * 2
	requestTimeout    = time.Second * 3
)

// RPCClient is used by LiveKit Server
type RPCClient interface {
	// GetUpdateChannel returns a subscription for ingress info updates
	GetUpdateChannel(ctx context.Context) (utils.PubSub, error)
	// GetEntityChannel returns a subscription for entity requests
	GetEntityChannel(ctx context.Context) (utils.PubSub, error)
	// SendRequest sends a request to all available instances
	SendRequest(ctx context.Context, req *livekit.IngressRequest) (*livekit.IngressState, error)
	// SendResponse returns a GetIngressInfo response
	SendGetIngressInfoResponse(ctx context.Context, req *livekit.GetIngressInfoRequest, resp *livekit.GetIngressInfoResponse, err error) error
}

// RPCServer is used by Ingress
type RPCServer interface {
	// IngressSubscription subscribes to requests for a specific ingress ID
	IngressSubscription(ctx context.Context, ingressID string) (utils.PubSub, error)
	// SendResponse returns an RPC response
	SendResponse(ctx context.Context, request *livekit.IngressRequest, state *livekit.IngressState, err error) error
	// SendUpdate sends an ingress info update
	SendUpdate(ctx context.Context, ingressId string, state *livekit.IngressState) error
	// SendGetIngressInfoRequest sends a request to all available instances
	SendGetIngressInfoRequest(ctx context.Context, req *livekit.GetIngressInfoRequest) (*livekit.GetIngressInfoResponse, error)
}

type RPC interface {
	RPCClient
	RPCServer
}

type RedisRPC struct {
	nodeID livekit.NodeID
	bus    *utils.RedisMessageBus
}

func NewRedisRPC(nodeID livekit.NodeID, rc redis.UniversalClient) RPC {
	if rc == nil {
		return nil
	}

	bus := utils.NewRedisMessageBus(rc)
	return &RedisRPC{
		nodeID: nodeID,
		bus:    bus.(*utils.RedisMessageBus),
	}
}

func (r *RedisRPC) GetUpdateChannel(ctx context.Context) (utils.PubSub, error) {
	return r.bus.SubscribeQueue(context.Background(), updateChannel)
}

func (r *RedisRPC) sendRequest(
	ctx context.Context,
	requestID string,
	channel string,
	request proto.Message,
	resp proto.Message) (proto.Message, error) {
	sub, err := r.bus.Subscribe(ctx, responseChannel(requestID))
	if err != nil {
		return nil, err
	}
	defer func() {
		err := sub.Close()
		if err != nil {
			logger.Errorw("failed to unsubscribe from response channel", err)
		}
	}()

	err = r.bus.Publish(ctx, channel, request)
	if err != nil {
		return nil, err
	}

	select {
	case raw := <-sub.Channel():
		err := proto.Unmarshal(sub.Payload(raw), resp)
		if err != nil {
			return nil, err
		} else {
			return resp, nil
		}

	case <-time.After(requestTimeout):
		return nil, ErrNoResponse
	}
}

func (r *RedisRPC) SendRequest(ctx context.Context, req *livekit.IngressRequest) (*livekit.IngressState, error) {
	requestID := utils.NewGuid(utils.RPCPrefix)
	var channel string
	var err error

	req.RequestId = requestID
	req.SenderId = string(r.nodeID)
	channel = requestChannel(req.IngressId)
	resp := &livekit.IngressResponse{}

	_, err = r.sendRequest(ctx, requestID, channel, req, resp)
	if err != nil {
		return nil, err
	} else if resp.Error != "" {
		return nil, errors.New(resp.Error)
	} else {
		return resp.State, nil
	}
}

func (r *RedisRPC) SendGetIngressInfoRequest(ctx context.Context, req *livekit.GetIngressInfoRequest) (*livekit.GetIngressInfoResponse, error) {
	requestID := utils.NewGuid(utils.RPCPrefix)
	var channel string
	var err error

	req.RequestId = requestID
	req.SenderId = string(r.nodeID)
	req.SentAt = time.Now().UnixNano()
	channel = entityChannel
	resp := &livekit.GetIngressInfoResponse{}

	_, err = r.sendRequest(ctx, requestID, channel, req, resp)
	if err != nil {
		return nil, err
	} else if resp.Error != "" {
		return nil, errors.New(resp.Error)
	} else {
		return resp, nil
	}
}
func (r *RedisRPC) IngressSubscription(ctx context.Context, ingressID string) (utils.PubSub, error) {
	return r.bus.Subscribe(ctx, requestChannel(ingressID))
}

func (r *RedisRPC) SendResponse(ctx context.Context, req *livekit.IngressRequest, state *livekit.IngressState, err error) error {
	res := &livekit.IngressResponse{
		State:     state,
		RequestId: req.RequestId,
	}

	if err != nil {
		res.Error = err.Error()
	}

	return r.bus.Publish(ctx, responseChannel(res.RequestId), res)
}

func (r *RedisRPC) SendGetIngressInfoResponse(ctx context.Context, req *livekit.GetIngressInfoRequest, resp *livekit.GetIngressInfoResponse, err error) error {
	resp.RequestId = req.RequestId

	if err != nil {
		resp.Error = err.Error()
	}

	return r.bus.Publish(ctx, responseChannel(req.RequestId), resp)
}

func (r *RedisRPC) SendUpdate(ctx context.Context, ingressId string, state *livekit.IngressState) error {
	return r.bus.Publish(ctx, updateChannel, &livekit.UpdateIngressStateRequest{
		IngressId: ingressId,
		State:     state,
	})
}

func (r *RedisRPC) GetEntityChannel(ctx context.Context) (utils.PubSub, error) {
	return r.bus.SubscribeQueue(ctx, entityChannel)
}

func requestChannel(ingressID string) string {
	return requestChannelPrefix + ingressID
}

func responseChannel(nodeID string) string {
	return responseChannelPrefix + nodeID
}

type HandlerServerImpl interface {
	rpc.IngressHandlerServerImpl
	rpc.IngressUpdateServerImpl
}

type HandlerServer interface {
	rpc.IngressHandlerServer
	rpc.IngressUpdateServer
	SetServerImpl(HandlerServerImpl) error
}

type handlerServer struct {
	nodeID livekit.NodeID
	bus    psrpc.MessageBus
	rpc.IngressHandlerServer
	rpc.IngressUpdateServer
}

func NewHandlerServer(nodeID livekit.NodeID, bus psrpc.MessageBus) HandlerServer {
	return &handlerServer{
		nodeID: nodeID,
		bus:    bus,
	}
}

func (s *handlerServer) SetServerImpl(impl HandlerServerImpl) error {
	serverID := string(s.nodeID)
	handlerServer, err := rpc.NewIngressHandlerServer(serverID, impl, s.bus)
	if err != nil {
		return err
	}
	updateServer, err := rpc.NewIngressUpdateServer(serverID, impl, s.bus)
	if err != nil {
		return err
	}

	s.IngressHandlerServer = handlerServer
	s.IngressUpdateServer = updateServer

	return nil
}

func (s *handlerServer) Kill() {
	s.IngressHandlerServer.Kill()
	s.IngressUpdateServer.Kill()
}

func (s *handlerServer) Shutdown() {
	s.IngressHandlerServer.Shutdown()
	s.IngressUpdateServer.Shutdown()
}

type InternalServerImpl interface {
	rpc.IngressInternalServerImpl
	rpc.IngressUpdateServerImpl
}

type InternalServer interface {
	rpc.IngressInternalServer
	rpc.IngressUpdateServer
	rpc.IngressEntityClient
	SetServerImpl(InternalServerImpl) error
}

type internalServer struct {
	nodeID livekit.NodeID
	bus    psrpc.MessageBus
	rpc.IngressInternalServer
	rpc.IngressUpdateServer
	rpc.IngressEntityClient
}

func NewInternalServer(nodeID livekit.NodeID, bus psrpc.MessageBus) (InternalServer, error) {
	entityClient, err := rpc.NewIngressEntityClient(string(nodeID), bus)
	if err != nil {
		return nil, err
	}

	return &internalServer{
		nodeID:              nodeID,
		bus:                 bus,
		IngressEntityClient: entityClient,
	}, nil
}

func (s *internalServer) SetServerImpl(impl InternalServerImpl) error {
	serverID := string(s.nodeID)
	internalServer, err := rpc.NewIngressInternalServer(serverID, impl, s.bus)
	if err != nil {
		return err
	}
	updateServer, err := rpc.NewIngressUpdateServer(serverID, impl, s.bus)
	if err != nil {
		return err
	}

	s.IngressInternalServer = internalServer
	s.IngressUpdateServer = updateServer

	return nil
}

func (s *internalServer) Kill() {
	s.IngressInternalServer.Kill()
	s.IngressUpdateServer.Kill()
}

func (s *internalServer) Shutdown() {
	s.IngressInternalServer.Shutdown()
	s.IngressUpdateServer.Shutdown()
}
