package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	_ "unsafe"

	"gitee.com/baixudong/gospider/http2"
	"gitee.com/baixudong/gospider/ja3"
	"gitee.com/baixudong/gospider/tools"
	"gitee.com/baixudong/gospider/websocket"
)

func (obj *Client) wsSend(ctx context.Context, wsClient *websocket.Conn, wsServer *websocket.Conn) (err error) {
	defer wsServer.Close("close")
	defer wsClient.Close("close")
	var msgType websocket.MessageType
	var msgData []byte
	for {
		if msgType, msgData, err = wsClient.Recv(ctx); err != nil {
			return
		}
		if obj.wsCallBack != nil {
			if err = obj.wsCallBack(msgType, msgData, Send); err != nil {
				return err
			}
		}
		if err = wsServer.Send(ctx, msgType, msgData); err != nil {
			return
		}
	}
}
func (obj *Client) wsRecv(ctx context.Context, wsClient *websocket.Conn, wsServer *websocket.Conn) (err error) {
	defer wsServer.Close("close")
	defer wsClient.Close("close")
	var msgType websocket.MessageType
	var msgData []byte
	for {
		if msgType, msgData, err = wsServer.Recv(ctx); err != nil {
			return
		}
		if obj.wsCallBack != nil {
			if err = obj.wsCallBack(msgType, msgData, Recv); err != nil {
				return err
			}
		}
		if err = wsClient.Send(ctx, msgType, msgData); err != nil {
			return
		}
	}
}

type erringRoundTripper interface {
	RoundTripErr() error
}

func (obj *Client) http12Copy(ctx context.Context, client *ProxyConn, server *ProxyConn) (err error) {
	defer client.Close()
	defer server.Close()
	serverConn := http2.Upg{
		H2Ja3Spec: client.option.h2Ja3Spec,
	}.UpgradeFn(server.option.host, server.conn.(*tls.Conn))
	if erringRoundTripper, ok := serverConn.(erringRoundTripper); ok && erringRoundTripper.RoundTripErr() != nil {
		return erringRoundTripper.RoundTripErr()
	}
	var req *http.Request
	var resp *http.Response
	for {
		if client.req != nil {
			req, client.req = client.req, nil
		} else {
			if req, err = client.readRequest(server.option.ctx, obj.requestCallBack); err != nil {
				return
			}
		}
		req = req.WithContext(client.option.ctx)
		req.Proto = "HTTP/2.0"
		req.ProtoMajor = 2
		req.ProtoMinor = 0
		if resp, err = serverConn.RoundTrip(req); err != nil {
			return
		}
		resp.Proto = "HTTP/1.1"
		if resp.ContentLength <= 0 {
			resp.TransferEncoding = []string{"chunked"}
		}
		resp.ProtoMajor = 1
		resp.ProtoMinor = 1
		resp.Request = req.WithContext(server.option.ctx)
		if obj.responseCallBack != nil {
			if err = obj.responseCallBack(req, resp); err != nil {
				return
			}
		}
		if err = resp.Write(client); err != nil {
			return
		}
	}
}
func (obj *Client) http11Copy(ctx context.Context, client *ProxyConn, server *ProxyConn) (err error) {
	defer client.Close()
	defer server.Close()
	var req *http.Request
	var rsp *http.Response
	for !server.option.isWs {
		if client.req != nil {
			req, client.req = client.req, nil
		} else {
			if req, err = client.readRequest(server.option.ctx, obj.requestCallBack); err != nil {
				return
			}
		}
		req = req.WithContext(client.option.ctx)
		if err = req.Write(server); err != nil {
			return
		}
		if rsp, err = server.readResponse(req); err != nil {
			return
		}
		rsp.Request = req.WithContext(server.option.ctx)
		if obj.responseCallBack != nil {
			if err = obj.responseCallBack(req, rsp); err != nil {
				return
			}
		}
		if err = rsp.Write(client); err != nil {
			return
		}
	}
	return
}

func (obj *Client) copyMain(ctx context.Context, client *ProxyConn, server *ProxyConn) (err error) {
	if client.option.schema == "http" {
		return obj.copyHttpMain(ctx, client, server)
	} else if client.option.schema == "https" {
		if obj.requestCallBack != nil ||
			obj.responseCallBack != nil ||
			obj.wsCallBack != nil ||
			client.option.ja3 ||
			client.option.h2Ja3 ||
			client.option.method != http.MethodConnect {
			return obj.copyHttpsMain(ctx, client, server)
		}
		return obj.copyHttpMain(ctx, client, server)
	} else {
		return errors.New("schema error")
	}
}
func (obj *Client) copyHttpMain(ctx context.Context, client *ProxyConn, server *ProxyConn) (err error) {
	defer server.Close()
	defer client.Close()
	if client.option.http2 && !server.option.http2 { //http21 逻辑
		return errors.New("没有http2 to http1.1 的逻辑")
	}
	if !client.option.http2 && server.option.http2 { //http12 逻辑
		return obj.http12Copy(ctx, client, server)
	}
	if client.option.http2 && server.option.http2 {
		go func() {
			defer client.Close()
			defer server.Close()
			tools.CopyWitchContext(ctx, client, server)
		}()
		return tools.CopyWitchContext(ctx, server, client)
	}
	if obj.responseCallBack == nil && obj.wsCallBack == nil && obj.requestCallBack == nil { //没有回调直接返回
		if client.req != nil {
			if err = client.req.Write(server); err != nil {
				return err
			}
			client.req = nil
		}
		go func() {
			defer client.Close()
			defer server.Close()
			err = tools.CopyWitchContext(ctx, client, server)
		}()
		err = tools.CopyWitchContext(ctx, server, client)
		return
	}
	if err = obj.http11Copy(ctx, client, server); err != nil { //http11 开始回调
		return err
	}
	if obj.wsCallBack == nil { //没有ws 回调直接返回
		go func() {
			defer client.Close()
			defer server.Close()
			tools.CopyWitchContext(ctx, client, server)
		}()
		return tools.CopyWitchContext(ctx, server, client)
	}
	//ws 开始回调
	wsClient := websocket.NewConn(client, false, client.option.wsOption)
	wsServer := websocket.NewConn(server, true, server.option.wsOption)
	defer wsServer.Close("close")
	defer wsClient.Close("close")
	go obj.wsRecv(ctx, wsClient, wsServer)
	return obj.wsSend(ctx, wsClient, wsServer)
}
func (obj *Client) copyHttpsMain(ctx context.Context, client *ProxyConn, server *ProxyConn) (err error) {
	tlsServer, http2, certs, err := obj.tlsServer(ctx, server, client.option.host, client.option.isWs || server.option.isWs, client.option.ja3, client.option.ja3Spec)
	if err != nil {
		return err
	}
	server.option.http2 = http2
	if client.option.method != http.MethodConnect {
		return obj.copyHttpMain(ctx, client, newProxyCon(ctx, tlsServer, bufio.NewReader(tlsServer), *server.option, false))
	}
	var cert tls.Certificate
	if len(certs) > 0 {
		cert, err = tools.GetProxyCertWithCert(obj.crt, obj.key, certs[0])
	} else {
		cert, err = tools.GetProxyCertWithName(tools.GetServerName(client.option.host))
	}
	if err != nil {
		return err
	}
	clientH2 := server.option.http2
	//如果服务端是h2,需要拦截请求 或需要设置h2指纹，就走12
	if clientH2 && (obj.responseCallBack != nil || obj.requestCallBack != nil || client.option.h2Ja3) {
		clientH2 = false
	}
	tlsClient, http2, err := obj.tlsClient(ctx, client, !clientH2, cert) //服务端为h2时，客户端随意，服务端为h1时，客户端必须为h1
	if err != nil {
		return err
	}
	client.option.http2 = http2
	clientProxy := newProxyCon(ctx, tlsClient, bufio.NewReader(tlsClient), *client.option, true)
	serverProxy := newProxyCon(ctx, tlsServer, bufio.NewReader(tlsServer), *server.option, false)
	return obj.copyHttpMain(ctx, clientProxy, serverProxy)
}
func (obj *Client) tlsClient(ctx context.Context, conn net.Conn, disHttp2 bool, cert tls.Certificate) (tlsConn *tls.Conn, http2 bool, err error) {
	var nextProtos []string
	if disHttp2 {
		nextProtos = []string{"http/1.1"}
	} else {
		nextProtos = []string{"h2", "http/1.1"}
	}
	tlsConn = tls.Server(conn, &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cert},
		NextProtos:         nextProtos,
	})
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		return nil, false, err
	}
	return tlsConn, tlsConn.ConnectionState().NegotiatedProtocol == "h2", err
}
func (obj *Client) tlsServer(ctx context.Context, conn net.Conn, addr string, disHttp2 bool, isJa3 bool, ja3Spec ja3.ClientHelloSpec) (net.Conn, bool, []*x509.Certificate, error) {
	if isJa3 {
		if tlsConn, err := ja3.NewClient(ctx, conn, ja3Spec, disHttp2, addr); err != nil {
			return tlsConn, false, nil, err
		} else {
			return tlsConn, tlsConn.ConnectionState().NegotiatedProtocol == "h2", tlsConn.ConnectionState().PeerCertificates, err
		}
	} else {
		var nextProtos []string
		if disHttp2 {
			nextProtos = []string{"http/1.1"}
		} else {
			nextProtos = []string{"h2", "http/1.1"}
		}
		tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: tools.GetServerName(addr), NextProtos: nextProtos})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return tlsConn, false, nil, err
		} else {
			return tlsConn, tlsConn.ConnectionState().NegotiatedProtocol == "h2", tlsConn.ConnectionState().PeerCertificates, err
		}
	}
}
