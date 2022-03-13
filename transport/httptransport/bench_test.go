package httptransport

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"testing"
	"time"

	logging "github.com/ipfs/go-log/v2"

	"github.com/multiformats/go-multiaddr"

	car2 "github.com/filecoin-project/boost/car"

	"github.com/filecoin-project/boost/testutil"

	"github.com/filecoin-project/boost/transport/types"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipfs/go-merkledag"
	"github.com/ipld/go-car"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/stretchr/testify/require"
)

type testServer interface {
	Request(authToken string) types.HttpRequest
	Client() *httpTransport
	Server() *Libp2pCarServer
	Stop() error
}

func TestBenchTransport(t *testing.T) {
	ctx := context.Background()

	//logging.SetLogLevel("*", "debug")
	//logging.SetLogLevel("blockservice", "warn")

	//rawSize := 1024 * 1024 * 1024
	//rawSize := 256 * 1024 * 1024
	rawSize := 64 * 1024 * 1024
	//rawSize := 2 * 1024 * 1024
	t.Logf("Benchmark file of size %d (%.2f MB)", rawSize, float64(rawSize)/(1024*1024))

	rseed := 5
	source := io.LimitReader(rand.New(rand.NewSource(int64(rseed))), int64(rawSize))
	ds := dssync.MutexWrap(datastore.NewMapDatastore())
	bs := bstore.NewBlockstore(ds)
	bserv := blockservice.New(bs, nil)
	dserv := merkledag.NewDAGService(bserv)

	t.Log("starting import")
	importStart := time.Now()
	nd, err := DagImport(dserv, source)
	require.NoError(t, err)
	t.Logf("import took %s", time.Since(importStart))

	// Get the size of the CAR file
	t.Log("getting car file size")
	carSizeStart := time.Now()
	cw := &testutil.CountWriter{}
	err = car.WriteCar(context.Background(), dserv, []cid.Cid{nd.Cid()}, cw)
	require.NoError(t, err)
	t.Logf("car size: %d bytes (%.2f MB) - took %s", cw.Total, float64(cw.Total)/(1024*1024), time.Since(carSizeStart))

	performTransfer := func(t *testing.T, ts testServer, authDB *AuthTokenDB) {
		defer ts.Stop() //nolint:errcheck

		// Create an auth token
		id := "1"
		authToken, err := GenerateAuthToken()
		require.NoError(t, err)
		carSize := cw.Total
		proposalCid, err := cid.Parse("bafkqaaa")
		require.NoError(t, err)
		err = authDB.Put(ctx, authToken, AuthValue{
			ID:          id,
			ProposalCid: proposalCid,
			PayloadCid:  nd.Cid(),
			Size:        uint64(carSize),
		})
		require.NoError(t, err)

		// Perform retrieval with the auth token
		req := ts.Request(authToken)
		of := getTempFilePath(t)

		client := ts.Client()
		logging.SetLogLevel("boost-storage-deal", "debug")
		logging.SetLogLevel("http-transport", "debug")

		xferStart := time.Now()
		th := executeTransfer(t, ctx, client, carSize, req, of)
		require.NotNil(t, th)

		// Wait for the transfer to complete
		clientEvts := waitForTransferComplete(th)
		end := time.Now()

		require.NotEmpty(t, clientEvts)
		lastClientEvt := clientEvts[len(clientEvts)-1]
		require.EqualValues(t, carSize, lastClientEvt.NBytesReceived)

		xferTime := end.Sub(xferStart)
		mbPerS := (float64(cw.Total) / (1024 * 1024)) / (float64(xferTime) / float64(time.Second))
		t.Logf("transfer of %.2f MB took %s: %.2f MB / s", float64(cw.Total)/(1024*1024), xferTime, mbPerS)
	}

	t.Run("http over libp2p", func(t *testing.T) {
		srvMA, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/5701")
		clientHost, srvHost := setupBenchLibp2pHosts(t, srvMA)
		defer srvHost.Close()
		defer clientHost.Close()

		authDB := NewAuthTokenDB(ds)
		srv := NewLibp2pCarServer(srvHost, authDB, bs, ServerConfig{
			BlockInfoCacheManager: car2.NewDelayedUnrefBICM(time.Minute),
		})
		err = srv.Start(ctx)
		require.NoError(t, err)

		bsrv := &benchLibp2pHttpServer{
			t:          t,
			srvHost:    srvHost,
			clientHost: clientHost,
			srv:        srv,
		}

		performTransfer(t, bsrv, authDB)
	})

	t.Run("raw http", func(t *testing.T) {
		srvMA, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9000")
		clientHost, srvHost := setupBenchLibp2pHosts(t, srvMA)
		defer srvHost.Close()
		defer clientHost.Close()

		authDB := NewAuthTokenDB(ds)
		srv := NewLibp2pCarServer(srvHost, authDB, bs, ServerConfig{
			BlockInfoCacheManager: car2.NewDelayedUnrefBICM(time.Minute),
		})
		srv.ctx, srv.cancel = context.WithCancel(ctx)
		srv.transfersMgr.start(ctx)

		http.HandleFunc("/", srv.handler)
		listenAddr := "127.0.0.1:5701"
		go func() {
			_ = http.ListenAndServe(listenAddr, nil)
		}()

		bsrv := &benchRawHttpServer{
			t:          t,
			srvHost:    srvHost,
			clientHost: clientHost,
			srv:        srv,
			listenAddr: listenAddr,
		}

		performTransfer(t, bsrv, authDB)
	})
}

func setupBenchLibp2pHosts(t *testing.T, srvMA multiaddr.Multiaddr) (host.Host, host.Host) {
	clientMA, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/0")
	clientHost := newHost(t, clientMA)
	srvHost := newHost(t, srvMA)

	//clientHost.Peerstore().AddAddrs(srvHost.ID(), srvHost.Addrs(), peerstore.PermanentAddrTTL)
	//srvHost.Peerstore().AddAddrs(clientHost.ID(), clientHost.Addrs(), peerstore.PermanentAddrTTL)

	return clientHost, srvHost
}

type benchRawHttpServer struct {
	t          *testing.T
	srvHost    host.Host
	clientHost host.Host
	srv        *Libp2pCarServer
	listenAddr string
}

func (s *benchRawHttpServer) Request(authToken string) types.HttpRequest {
	return types.HttpRequest{
		URL: "http://" + s.listenAddr,
		Headers: map[string]string{
			"Authorization": BasicAuthHeader("", authToken),
		},
	}
}

func (s *benchRawHttpServer) Client() *httpTransport {
	return New(s.clientHost, newDealLogger(s.t, context.Background()))
}

func (s *benchRawHttpServer) Server() *Libp2pCarServer {
	return s.srv
}

func (s *benchRawHttpServer) Stop() error {
	return nil
}

type benchLibp2pHttpServer struct {
	t          *testing.T
	srvHost    host.Host
	clientHost host.Host
	srv        *Libp2pCarServer
}

func (s *benchLibp2pHttpServer) Request(authToken string) types.HttpRequest {
	//return types.HttpRequest{
	//	URL: "libp2p:///ip4/127.0.0.1/tcp/5700/p2p/" + s.srvHost.ID().Pretty(),
	//	Headers: map[string]string{
	//		"Authorization": BasicAuthHeader("", authToken),
	//	},
	//}
	return newLibp2pHttpRequest(s.srvHost, authToken)
}

func (s *benchLibp2pHttpServer) Client() *httpTransport {
	return New(s.clientHost, newDealLogger(s.t, context.Background()))
}

func (s *benchLibp2pHttpServer) Server() *Libp2pCarServer {
	return s.srv
}

func (s *benchLibp2pHttpServer) Stop() error {
	return s.srv.Stop(context.Background())
}
