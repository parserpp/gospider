package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitee.com/baixudong/gospider/kinds"
	"gitee.com/baixudong/gospider/requests"
	"gitee.com/baixudong/gospider/thread"
	"gitee.com/baixudong/gospider/tools"
)

type ClientOption struct {
	Usr       string      //用户名
	Pwd       string      //密码
	IpWhite   []net.IP    //白名单 192.168.1.1,192.168.1.2
	Dialer    *net.Dialer //连接的Dialer
	LocalAddr string      //本地网卡出口
	Port      int         //代理端口
	Host      string      //代理host
}
type netDial struct {
	dialer *net.Dialer //连接的Dialer
}

func (obj *netDial) DialContext(ctx context.Context, network string, address string) (net.Conn, error) { //http conn
	return obj.dialer.DialContext(ctx, network, address)
}
func (obj *netDial) Dial(network string, address string) (net.Conn, error) { //websock conn
	return obj.dialer.Dial(network, address)
}

type Client struct {
	Proxy     string                 //代理ip 192.168.1.50:8888
	GetProxy  func() (string, error) //代理ip 116.62.55.139:8888
	Debug     bool                   //是否打印debug
	Err       error                  //错误
	DisVerify bool                   //关闭验证

	dialer   *netDial     //连接的Dialer
	listener net.Listener //Listener 服务
	basic    string
	usr      string
	pwd      string
	verify   bool
	ipWhite  *kinds.Set[string]
	ctx      context.Context
	cnl      context.CancelFunc
}

func NewClient(pre_ctx context.Context, options ...ClientOption) (*Client, error) {
	var option ClientOption
	if len(options) > 0 {
		option = options[0]
	}
	if pre_ctx == nil {
		pre_ctx = context.TODO()
	}
	ctx, cnl := context.WithCancel(pre_ctx)
	server := Client{}
	server.ctx = ctx
	server.cnl = cnl
	if option.Usr != "" && option.Pwd != "" {
		server.basic = "Basic " + tools.Base64Encode(option.Usr+":"+option.Pwd)
		server.usr = option.Usr
		server.pwd = option.Pwd
		server.verify = true
	}
	server.ipWhite = kinds.NewSet[string]()
	for _, ip_white := range option.IpWhite {
		server.ipWhite.Add(ip_white.String())
	}
	if option.Dialer == nil {
		option.Dialer = &net.Dialer{
			Timeout:   time.Duration(8) * time.Second,
			KeepAlive: time.Duration(10) * time.Second,
		}
	}
	if option.LocalAddr != "" {
		if !strings.Contains(option.LocalAddr, ":") {
			option.LocalAddr += ":0"
		}
		localaddr, err := net.ResolveTCPAddr("tcp", option.LocalAddr)
		if err != nil {
			return nil, err
		}
		option.Dialer.LocalAddr = localaddr
	}
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", option.Host, option.Port)) //监听本地端口
	if err != nil {
		return nil, err
	}
	server.listener = l
	server.dialer = &netDial{
		dialer: option.Dialer,
	}
	return &server, nil
}

// 代理监听的端口
func (obj *Client) Addr() string {
	return obj.listener.Addr().String()
}

func (obj *Client) Run() error {
	defer obj.Close()
	pool := thread.NewClient(obj.ctx, 65535)
	pool.Debug = obj.Debug
	for {
		select {
		case <-obj.ctx.Done():
			obj.Err = obj.ctx.Err()
			return obj.Err
		default:
			client, err := obj.listener.Accept() //接受数据
			if err != nil {
				obj.Err = err
				return err
			}
			if _, err = pool.Write(&thread.Task{
				Func: obj.mainHandle,
				Args: []any{client},
			}); err != nil {
				obj.Err = err
				return obj.Err
			}
		}
	}
}
func (obj *Client) Close() {
	obj.listener.Close()
	obj.cnl()
}
func (obj *Client) Done() <-chan struct{} {
	return obj.ctx.Done()
}

func (obj *Client) whiteVerify(client net.Conn) bool {
	if obj.DisVerify {
		return true
	}
	host, _, err := net.SplitHostPort(client.RemoteAddr().String())
	if err != nil || !obj.ipWhite.Has(host) {
		return false
	}
	return true
}

// 返回:请求所有内容,第一行的内容被" "分割的数组,第一行的内容,error
func (obj *Client) verifyPwd(client net.Conn, clientReq *http.Request) error {
	if obj.verify && clientReq.Header.Get("Proxy-Authorization") != obj.basic && !obj.whiteVerify(client) { //验证密码是否正确
		client.Write([]byte(fmt.Sprintf("%s 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic\r\n\r\n", clientReq.Proto)))
		return errors.New("auth verify fail")
	}
	return nil
}
func (obj *Client) getHttpProxyConn(ctx context.Context, ipUrl *url.URL) (net.Conn, error) {
	return obj.dialer.DialContext(ctx, "tcp", net.JoinHostPort(ipUrl.Hostname(), ipUrl.Port()))
}

func (obj *Client) mainHandle(ctx context.Context, client net.Conn) error {
	if client == nil {
		return errors.New("client is nil")
	}
	defer client.Close()
	if !obj.verify && !obj.whiteVerify(client) {
		return errors.New("auth verify false")
	}
	var err error
	clientReader := bufio.NewReader(client)
	firstCons, err := clientReader.Peek(1)
	if err != nil {
		return err
	}
	if firstCons[0] == 5 {
		return obj.sockes5Handle(ctx, client, clientReader)
	}
	return obj.httpHandle(ctx, client, clientReader)
}
func (obj *Client) verifyProxy(ip_addr string) (*url.URL, error) {
	var err error
	var ipUrl *url.URL
	if ipUrl, err = url.Parse(ip_addr); err != nil {
		return ipUrl, err
	}
	if ipUrl.Scheme != "http" && ipUrl.Scheme != "socks5" {
		return ipUrl, errors.New("proxy scheme error")
	}
	return ipUrl, err
}

func readRequest(b *bufio.Reader) (*http.Request, bool, error) {
	clientReq, err := http.ReadRequest(b)
	if err != nil {
		return clientReq, false, err
	}
	if clientReq.URL.Hostname() == "" {
		clientReq.URL.Host = clientReq.Host
	}
	if clientReq.URL.Port() == "" {
		if clientReq.Method == http.MethodConnect {
			clientReq.URL.Host = clientReq.URL.Hostname() + ":" + "443"
		} else {
			clientReq.URL.Host = clientReq.URL.Hostname() + ":" + "80"
		}
	}
	if strings.HasPrefix(clientReq.Host, "127.0.0.1") || strings.HasPrefix(clientReq.Host, "localhost") {
		return clientReq, false, errors.New("loop addr error")
	}
	return clientReq, clientReq.Header.Get("Upgrade") == "websocket", err
}
func writeRequest(clientReq *http.Request, w io.Writer, ipUrl *url.URL) error {
	for key := range clientReq.Header {
		if strings.HasPrefix(key, "Proxy-") {
			clientReq.Header.Del(key)
		}
	}
	if ipUrl != nil && ipUrl.User != nil { //添加代理密码
		if _, ok := ipUrl.User.Password(); ok {
			clientReq.Header.Set("Proxy-Authorization", "Basic "+tools.Base64Encode(ipUrl.User.String()))
		}
	}
	return clientReq.Write(w)
}
func (obj *Client) httpHandle(ctx context.Context, client net.Conn, clientReader *bufio.Reader) error {
	defer client.Close()
	var err error
	clientReq, isWebsocket, err := readRequest(clientReader)
	if err != nil {
		log.Print(err)
		return err
	}
	log.Print("成功", clientReq.Method)
	log.Print("成功", clientReq.URL.Hostname())
	log.Print("成功", clientReq.URL.Port())
	if err = obj.verifyPwd(client, clientReq); err != nil {
		return err
	}
	var ip_addr string
	if obj.GetProxy != nil {
		ip_addr, err = obj.GetProxy()
		if err != nil {
			return err
		}
	} else if obj.Proxy != "" {
		ip_addr = obj.Proxy
	}
	var server net.Conn
	if ip_addr == "" { //使用本地转发的逻辑
		if server, err = obj.dialer.DialContext(ctx, "tcp", net.JoinHostPort(clientReq.URL.Hostname(), clientReq.URL.Port())); err != nil { //获取服务连接
			return err
		}
		defer server.Close()
		if clientReq.Method == http.MethodConnect {
			if _, err = client.Write([]byte(fmt.Sprintf("%s 200 Connection established\r\n\r\n", clientReq.Proto))); err != nil {
				return err
			}
		} else {
			if err = writeRequest(clientReq, server, nil); err != nil {
				return err
			}
		}
	} else { //使用代理转发的逻辑
		ipUrl, err := obj.verifyProxy(ip_addr)
		if err != nil {
			return err
		}
		switch ipUrl.Scheme {
		case "http":
			if server, err = obj.getHttpProxyConn(ctx, ipUrl); err != nil { //获取服务连接
				return err
			}
			defer server.Close()
			if err = writeRequest(clientReq, server, ipUrl); err != nil {
				return err
			}
		case "socks5":
			if server, err = requests.GetSocks5ProxyConn(ctx, obj.dialer, ipUrl, net.JoinHostPort(clientReq.URL.Hostname(), clientReq.URL.Port())); err != nil { //获取服务连接
				return err
			}
			defer server.Close()
			if clientReq.Method == http.MethodConnect {
				if _, err = client.Write([]byte(fmt.Sprintf("%s 200 Connection established\r\n\r\n", clientReq.Proto))); err != nil {
					return err
				}
			} else {
				if err = writeRequest(clientReq, server, nil); err != nil {
					return err
				}
			}
		default:
			return errors.New("不支持的代理协议")
		}
	}
	go func() { //服务端到客户端
		defer server.Close()
		defer client.Close()
		io.Copy(client, server)
	}()
	if clientReq.Method != http.MethodConnect && !isWebsocket {
		for {
			if clientReq, _, err = readRequest(clientReader); err != nil {
				return err
			}
			if err = writeRequest(clientReq, server, nil); err != nil {
				return err
			}
		}
	}
	_, err = io.Copy(server, client) //客户端发送服务端
	return err
}
func (obj *Client) getSocketAddr(clientReader *bufio.Reader) (string, error) {
	buf := make([]byte, 4)
	addr := ""
	_, err := io.ReadFull(clientReader, buf) //读取版本号，CMD，RSV ，ATYP ，ADDR ，PORT
	if err != nil {
		return addr, fmt.Errorf("read header failed:%w", err)
	}
	ver, cmd, atyp := buf[0], buf[1], buf[3]
	if ver != 5 {
		return addr, fmt.Errorf("not supported ver:%v", ver)
	}
	if cmd != 1 {
		return addr, fmt.Errorf("not supported cmd:%v", ver)
	}
	switch atyp {
	case 1: //ipv4地址
		if _, err = io.ReadFull(clientReader, buf); err != nil {
			return addr, fmt.Errorf("read atyp failed:%w", err)
		}
		addr = net.IPv4(buf[0], buf[1], buf[2], buf[3]).String()
	case 3: //域名
		hostSize, err := clientReader.ReadByte() //域名的长度
		if err != nil {
			return addr, fmt.Errorf("read hostSize failed:%w", err)
		}
		host := make([]byte, hostSize)
		if _, err = io.ReadFull(clientReader, host); err != nil {
			return addr, fmt.Errorf("read host failed:%w", err)
		}
		addr = tools.BytesToString(host)
	case 4: //IPv6地址
		host := make([]byte, 16)
		if _, err = io.ReadFull(clientReader, host); err != nil {
			return addr, fmt.Errorf("read atyp failed:%w", err)
		}
		addr = net.IP(host).String()
	default:
		return addr, errors.New("invalid atyp")
	}
	if _, err = io.ReadFull(clientReader, buf[:2]); err != nil { //读取端口号
		return addr, fmt.Errorf("read port failed:%w", err)
	}
	return fmt.Sprintf("%s:%d", addr, binary.BigEndian.Uint16(buf[:2])), nil
}
func (obj *Client) verifySocket(client net.Conn, clientReader *bufio.Reader) error {
	ver, err := clientReader.ReadByte() //读取第一个字节判断是否是socks5协议
	if err != nil {
		return fmt.Errorf("read ver failed:%w", err)
	}
	if ver != 5 {
		return fmt.Errorf("not supported ver:%v", ver)
	}
	methodSize, err := clientReader.ReadByte() //读取第二个字节,method 的长度，支持认证的方法数量
	if err != nil {
		return fmt.Errorf("read methodSize failed:%w", err)
	}
	methods := make([]byte, methodSize)
	if _, err = io.ReadFull(clientReader, methods); err != nil { //读取method，支持认证的方法
		return fmt.Errorf("read method failed:%w", err)
	}
	if obj.verify && !obj.whiteVerify(client) { //开始验证用户名密码
		if bytes.IndexByte(methods, 2) == -1 {
			return errors.New("不支持用户名密码验证")
		}
		_, err = client.Write([]byte{5, 2}) //告诉客户端要进行用户名密码验证
		if err != nil {
			return err
		}
		okVar, err := clientReader.ReadByte() //获取版本，通常为0x01
		if err != nil {
			return err
		}
		Len, err := clientReader.ReadByte() //获取用户名的长度
		if err != nil {
			return err
		}
		user := make([]byte, Len)
		if _, err = io.ReadFull(clientReader, user); err != nil {
			return err
		}
		if Len, err = clientReader.ReadByte(); err != nil { //获取密码的长度
			return err
		}
		pass := make([]byte, Len)
		if _, err = io.ReadFull(clientReader, pass); err != nil {
			return err
		}
		if tools.BytesToString(user) != obj.usr || tools.BytesToString(pass) != obj.pwd {
			client.Write([]byte{okVar, 0xff}) //用户名密码错误
			return errors.New("用户名密码错误")
		}
		_, err = client.Write([]byte{okVar, 0}) //协商成功
		return err
	}
	_, err = client.Write([]byte{5, 0}) //协商成功
	return err
}
func (obj *Client) sockes5Handle(ctx context.Context, client net.Conn, clientReader *bufio.Reader) error {
	defer client.Close()
	var err error
	if err = obj.verifySocket(client, clientReader); err != nil {
		return err
	}
	serverAddr, err := obj.getSocketAddr(clientReader)
	if err != nil {
		return err
	}
	var ip_addr string
	var httpsByte byte
	if obj.GetProxy != nil {
		if ip_addr, err = obj.GetProxy(); err != nil {
			return err
		}
	} else if obj.Proxy != "" {
		ip_addr = obj.Proxy
	}
	var server net.Conn
	if ip_addr == "" {
		if server, err = obj.dialer.DialContext(ctx, "tcp", serverAddr); err != nil { //获取服务连接
			return err
		}
		if _, err = client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); err != nil { //响应客户端连接成功
			return err
		}
	} else {
		ipUrl, err := obj.verifyProxy(ip_addr)
		if err != nil {
			return err
		}
		switch ipUrl.Scheme {
		case "http":
			if server, err = obj.getHttpProxyConn(ctx, ipUrl); err != nil { //获取服务连接
				return err
			}
			defer server.Close()
			if _, err = client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); err != nil { //响应客户端连接成功
				return err
			}

			httpsBytes, err := clientReader.Peek(1)
			if err != nil {
				return err
			}
			httpsByte = httpsBytes[0]
			if httpsByte == 22 {
				if err = requests.Http2httpsConn(ctx, ipUrl, serverAddr, serverAddr, server); err != nil {
					return err
				}
			} else {
				clientReq, _, err := readRequest(clientReader)
				if err != nil {
					return err
				}
				if err = writeRequest(clientReq, server, ipUrl); err != nil {
					return err
				}
			}
		case "socks5":
			if server, err = requests.GetSocks5ProxyConn(ctx, obj.dialer, ipUrl, serverAddr); err != nil { //获取服务连接
				return err
			}
			defer server.Close()
			if _, err = client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); err != nil { //响应客户端连接成功
				return err
			}
		default:
			return errors.New("代理协议不支持")
		}
	}
	go func() { //服务端到客户端
		defer server.Close()
		defer client.Close()
		io.Copy(client, server)
	}()
	if httpsByte == 22 {
		_, err = io.Copy(server, clientReader) //客户端发送服务端
	} else {
		_, err = io.Copy(server, client) //客户端发送服务端
	}
	return err
}
