package requests

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"gitee.com/baixudong/gospider/http2"
	"gitee.com/baixudong/gospider/ja3"
)

type ClientOption struct {
	GetProxy              func(ctx context.Context, url *url.URL) (string, error) //根据url 返回代理，支持https,http,socks5 代理协议
	Proxy                 string                                                  //设置代理,支持https,http,socks5 代理协议
	TLSHandshakeTimeout   int64                                                   //tls 超时时间,default:15
	ResponseHeaderTimeout int64                                                   //第一个response headers 接收超时时间,default:30
	DisCookie             bool                                                    //关闭cookies管理
	DisCompression        bool                                                    //关闭请求头中的压缩功能
	LocalAddr             string                                                  //本地网卡出口ip
	IdleConnTimeout       int64                                                   //空闲连接在连接池中的超时时间,default:90
	KeepAlive             int64                                                   //keepalive保活检测定时,default:30
	DnsCacheTime          int64                                                   //dns解析缓存时间60*30
	AddrType              AddrType                                                //优先使用的addr 类型
	GetAddrType           func(string) AddrType
	Dns                   string              //dns
	Ja3                   bool                //开启ja3
	Ja3Spec               ja3.ClientHelloSpec //指定ja3Spec,使用ja3.CreateSpecWithStr 或者ja3.CreateSpecWithId 生成
	H2Ja3                 bool                //开启h2指纹
	H2Ja3Spec             ja3.H2Ja3Spec       //h2指纹

	RedirectNum   int                                         //重定向次数,小于0为禁用,0:不限制
	DisDecode     bool                                        //关闭自动编码
	DisRead       bool                                        //关闭默认读取请求体
	DisUnZip      bool                                        //变比自动解压
	TryNum        int64                                       //重试次数
	BeforCallBack func(context.Context, *RequestOption) error //请求前回调的方法
	AfterCallBack func(context.Context, *Response) error      //请求后回调的方法
	ErrCallBack   func(context.Context, error) bool           //请求error回调
	Timeout       int64                                       //请求超时时间
	Headers       any                                         //请求头
	Bar           bool                                        //是否开启bar
}
type Client struct {
	proxy         string //全局代理
	http2Upg      *http2.Upg
	redirectNum   int                                         //重定向次数
	disDecode     bool                                        //关闭自动编码
	disRead       bool                                        //关闭默认读取请求体
	disUnZip      bool                                        //变比自动解压
	tryNum        int64                                       //重试次数
	beforCallBack func(context.Context, *RequestOption) error //请求前回调的方法
	afterCallBack func(context.Context, *Response) error      //请求后回调的方法
	errCallBack   func(context.Context, error) bool           //请求error回调
	timeout       int64                                       //请求超时时间
	headers       any                                         //请求头
	bar           bool                                        //是否开启bar

	disCookie bool
	client    *http.Client

	ctx context.Context
	cnl context.CancelFunc
}

// 新建一个请求客户端,发送请求必须创建哈
func NewClient(preCtx context.Context, options ...ClientOption) (*Client, error) {
	var isG bool
	if preCtx == nil {
		preCtx = context.TODO()
	} else {
		isG = true
	}
	ctx, cnl := context.WithCancel(preCtx)
	var option ClientOption
	//初始化参数
	if len(options) > 0 {
		option = options[0]
	}
	if option.IdleConnTimeout == 0 {
		option.IdleConnTimeout = 90
	}
	if option.KeepAlive == 0 {
		option.KeepAlive = 30
	}
	if option.TLSHandshakeTimeout == 0 {
		option.TLSHandshakeTimeout = 15
	}
	if option.ResponseHeaderTimeout == 0 {
		option.ResponseHeaderTimeout = 30
	}
	if option.DnsCacheTime == 0 {
		option.DnsCacheTime = 60 * 30
	}
	if option.Ja3Spec.IsSet() {
		option.Ja3 = true
	}
	dialClient, err := NewDail(DialOption{
		Ja3:                 option.Ja3,
		Ja3Spec:             option.Ja3Spec,
		TLSHandshakeTimeout: option.TLSHandshakeTimeout,
		DnsCacheTime:        option.DnsCacheTime,
		GetProxy:            option.GetProxy,
		Proxy:               option.Proxy,
		KeepAlive:           option.KeepAlive,
		LocalAddr:           option.LocalAddr,
		AddrType:            option.AddrType,
		GetAddrType:         option.GetAddrType,
		Dns:                 option.Dns,
	})
	if err != nil {
		cnl()
		return nil, err
	}
	var client http.Client
	//创建cookiesjar
	var jar *cookiejar.Jar
	if !option.DisCookie {
		if jar, err = cookiejar.New(nil); err != nil {
			cnl()
			return nil, err
		}
	}
	baseTransport := &http.Transport{
		MaxIdleConns:        655350,
		MaxConnsPerHost:     655350,
		MaxIdleConnsPerHost: 655350,
		ProxyConnectHeader: http.Header{
			"User-Agent": []string{UserAgent},
		},
		TLSHandshakeTimeout:   time.Second * time.Duration(option.TLSHandshakeTimeout),
		ResponseHeaderTimeout: time.Second * time.Duration(option.ResponseHeaderTimeout),
		DisableCompression:    option.DisCompression,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		IdleConnTimeout:       time.Duration(option.IdleConnTimeout) * time.Second, //空闲连接在连接池中的超时时间
		DialContext:           dialClient.requestHttpDialContext,
		DialTLSContext:        dialClient.requestHttpDialTlsContext,
		ForceAttemptHTTP2:     true,
		Proxy: func(r *http.Request) (*url.URL, error) {
			ctxData := r.Context().Value(keyPrincipalID).(*reqCtxData)
			ctxData.url, ctxData.host = r.URL, r.Host
			return nil, nil
		},
	}
	var http2Upg *http2.Upg
	if option.H2Ja3 || option.H2Ja3Spec.IsSet() {
		http2Upg = http2.NewUpg(baseTransport, http2.UpgOption{H2Ja3Spec: option.H2Ja3Spec, DialTLSContext: dialClient.requestHttp2DialTlsContext})
		baseTransport.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{
			"h2": func(authority string, c *tls.Conn) http.RoundTripper {
				return http2Upg.UpgradeFn(authority, c)
			},
		}
	}
	client.Transport = baseTransport
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		ctxData := req.Context().Value(keyPrincipalID).(*reqCtxData)
		if ctxData.redirectNum == 0 || ctxData.redirectNum >= len(via) {
			return nil
		}
		return http.ErrUseLastResponse
	}
	result := &Client{
		ctx:           ctx,
		cnl:           cnl,
		proxy:         option.Proxy,
		client:        &client,
		http2Upg:      http2Upg,
		disCookie:     option.DisCookie,
		redirectNum:   option.RedirectNum,
		disDecode:     option.DisDecode,
		disRead:       option.DisRead,
		disUnZip:      option.DisUnZip,
		tryNum:        option.TryNum,
		beforCallBack: option.BeforCallBack,
		afterCallBack: option.AfterCallBack,
		errCallBack:   option.ErrCallBack,
		timeout:       option.Timeout,
		headers:       option.Headers,
		bar:           option.Bar,
	}
	if isG {
		go func() {
			<-result.ctx.Done()
			result.Close()
		}()
	}
	return result, nil
}

// 重置客户端至初始状态
func (obj *Client) Reset() error {
	if obj.client.Jar != nil {
		if jar, err := cookiejar.New(nil); err != nil {
			return err
		} else {
			obj.client.Jar = jar
		}
	}
	obj.CloseIdleConnections()
	return nil
}

// 关闭客户端
func (obj *Client) Close() {
	obj.CloseIdleConnections()
	obj.cnl()
}

// 关闭客户端中的空闲连接
func (obj *Client) CloseIdleConnections() {
	obj.client.CloseIdleConnections()
	if obj.http2Upg != nil {
		obj.http2Upg.CloseIdleConnections()
	}
}

// 返回url 的cookies,也可以设置url 的cookies
func (obj *Client) Cookies(href string, cookies ...*http.Cookie) Cookies {
	if obj.client.Jar == nil {
		return nil
	}
	u, err := url.Parse(href)
	if err != nil {
		return nil
	}
	obj.client.Jar.SetCookies(u, cookies)
	return obj.client.Jar.Cookies(u)
}

// 清除cookies
func (obj *Client) ClearCookies() error {
	if obj.client.Jar == nil {
		return nil
	}
	if jar, err := cookiejar.New(nil); err != nil {
		return err
	} else {
		obj.client.Jar = jar
	}
	return nil
}
func (obj *Client) getClient(request_option RequestOption) *http.Client {
	if !request_option.DisCookie || obj.client.Jar == nil {
		return obj.client
	}
	return &http.Client{
		Transport:     obj.client.Transport,
		CheckRedirect: obj.client.CheckRedirect,
	}
}
