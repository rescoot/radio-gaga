package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	redis_ipc "github.com/librescoot/redis-ipc"
)

type routePlanRequest struct {
	Stops []routeStop `json:"stops"`
}

type routePlanReply struct {
	ID string `json:"id"`
}

func routePlanTestServer(t *testing.T, server *miniredis.Miniredis, onReplace func(routePlanRequest) (routePlanReply, error), onClear func(struct{}) (routePlanReply, error)) {
	t.Helper()
	ipc, err := redis_ipc.New(redis_ipc.WithURL(server.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	call := redis_ipc.NewCallServer(ipc, "settings:route-plan", redis_ipc.WithCallServerConcurrency(1))
	redis_ipc.RegisterCall(call, "plan.replace", onReplace)
	redis_ipc.RegisterCall(call, "plan.clear", onClear)
	call.Start()
	t.Cleanup(func() { call.Stop(); ipc.Close() })
}

func TestNavigateCommandsUseRoutePlanRPC(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()
	server.HSet("system", "capabilities", "cap:ext:nav=2:keycard:ota")

	var replacements [][]routeStop
	clears := 0
	routePlanTestServer(t, server, func(req routePlanRequest) (routePlanReply, error) {
		replacements = append(replacements, req.Stops)
		return routePlanReply{ID: "plan"}, nil
	}, func(struct{}) (routePlanReply, error) {
		clears++
		return routePlanReply{}, nil
	})

	params := map[string]interface{}{"waypoints": []interface{}{
		map[string]interface{}{"latitude": 52.51, "longitude": 13.41, "label": "Work"},
		map[string]interface{}{"lat": 52.52, "lon": 13.42, "name": "Home"},
	}}
	if err := handleNavigateRouteCommand(client, ctx, params); err != nil {
		t.Fatal(err)
	}
	if err := handleNavigateCommand(client, ctx, map[string]interface{}{"latitude": 53.0, "longitude": 14.0, "address": "Office"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := handleNavigateCommand(client, ctx, map[string]interface{}{}, ""); err != nil {
		t.Fatal(err)
	}
	if len(replacements) != 2 || len(replacements[0]) != 2 || replacements[0][0] != (routeStop{52.51, 13.41, "Work"}) || replacements[0][1] != (routeStop{52.52, 13.42, "Home"}) || len(replacements[1]) != 1 || replacements[1][0] != (routeStop{53, 14, "Office"}) || clears != 1 {
		t.Fatalf("RPC requests: replacements=%+v clears=%d", replacements, clears)
	}
	if server.Exists("navigation") {
		t.Fatal("client wrote navigation projection")
	}
}

func TestNavigateRouteRequiresAdvertisedCapability(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	params := map[string]interface{}{"waypoints": []interface{}{
		map[string]interface{}{"lat": 52.0, "lon": 13.0},
	}}
	for _, capabilities := range []string{"", "cap:ext:nav=1", "cap:ext:nav=20", "nav=2"} {
		server.HSet("system", "capabilities", capabilities)
		if err := handleNavigateRouteCommand(client, context.Background(), params); err == nil {
			t.Error("accepted route without advertised nav=2 capability")
		}
	}
}

func TestNavigateRouteRejectsInvalidStopsWithoutRPC(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	server.HSet("system", "capabilities", "cap:ext:nav=2:keycard:ota")
	for _, raw := range []interface{}{
		nil, []interface{}{}, []interface{}{map[string]interface{}{"latitude": 91.0, "longitude": 13.0}},
		[]interface{}{map[string]interface{}{"latitude": 52.0}},
		[]interface{}{map[string]interface{}{"lat": "NaN", "lon": 13.0}},
		[]interface{}{map[string]interface{}{"lat": 52.0, "lon": 13.0, "label": strings.Repeat("x", 201)}},
		make([]interface{}, 26),
	} {
		if err := handleNavigateRouteCommand(client, context.Background(), map[string]interface{}{"waypoints": raw}); err == nil {
			t.Errorf("accepted invalid waypoints: %v", raw)
		}
	}
}

func TestNavigateCommandPropagatesRoutePlanError(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	routePlanTestServer(t, server, func(routePlanRequest) (routePlanReply, error) {
		return routePlanReply{}, errors.New("snapshot unavailable")
	}, func(struct{}) (routePlanReply, error) {
		return routePlanReply{}, errors.New("snapshot unavailable")
	})
	for _, params := range []map[string]interface{}{{"latitude": 52.0, "longitude": 13.0}, {}} {
		if err := handleNavigateCommand(client, context.Background(), params, ""); err == nil || !strings.Contains(err.Error(), "snapshot unavailable") {
			t.Fatalf("RPC error = %v", err)
		}
		if server.Exists("navigation") {
			t.Fatal("failed RPC caused direct navigation write")
		}
	}
}

func TestNavigateCommandsFailWithoutRoutePlanService(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	server.HSet("system", "capabilities", "cap:ext:nav=2")
	for _, run := range []func() error{
		func() error {
			return handleNavigateCommand(client, context.Background(), map[string]interface{}{"latitude": 52.0, "longitude": 13.0}, "")
		},
		func() error { return handleNavigateCommand(client, context.Background(), map[string]interface{}{}, "") },
		func() error {
			return handleNavigateRouteCommand(client, context.Background(), map[string]interface{}{"waypoints": []interface{}{map[string]interface{}{"lat": 52.0, "lon": 13.0}}})
		},
	} {
		if err := run(); err == nil {
			t.Fatal("missing service did not fail")
		}
		if server.Exists("navigation") {
			t.Fatal("missing service caused direct navigation write")
		}
	}
}
