package requests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	_ "unsafe"

	"gitee.com/baixudong/gospider/re"
	"gitee.com/baixudong/gospider/tools"
	"gitee.com/baixudong/gospider/websocket"
)

const keyPrincipalID = "gospiderContextData"

var (
	ErrFatal = errors.New("致命错误")
)

type reqCtxData struct {
	isCallback  bool
	proxy       *url.URL
	url         *url.URL
	host        string
	rawAddr     string
	rawHost     string
	rawMd5      string
	redirectNum int
	disProxy    bool
	ws          bool
}

func (obj *Client) Get(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodGet, href, options...)
}
func (obj *Client) Head(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodHead, href, options...)
}
func (obj *Client) Post(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodPost, href, options...)
}
func (obj *Client) Put(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodPut, href, options...)
}
func (obj *Client) Patch(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodPatch, href, options...)
}
func (obj *Client) Delete(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodDelete, href, options...)
}
func (obj *Client) Connect(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodConnect, href, options...)
}
func (obj *Client) Options(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodOptions, href, options...)
}
func (obj *Client) Trace(preCtx context.Context, href string, options ...RequestOption) (*Response, error) {
	return obj.Request(preCtx, http.MethodTrace, href, options...)
}

// 发送请求
func (obj *Client) Request(preCtx context.Context, method string, href string, options ...RequestOption) (resp *Response, err error) {
	if obj == nil {
		return nil, errors.New("初始化client失败")
	}
	if preCtx == nil {
		preCtx = obj.ctx
	}
	var rawOption RequestOption
	if len(options) > 0 {
		rawOption = options[0]
	}
	if rawOption.Method == "" {
		rawOption.Method = method
	}
	if rawOption.Url == nil {
		if rawOption.Url, err = url.Parse(href); err != nil {
			return
		}
	}
	if rawOption.Body != nil {
		if rawOption.Raw, err = io.ReadAll(rawOption.Body); err != nil {
			return
		}
	}
	var optionBak RequestOption
	if optionBak, err = obj.newRequestOption(rawOption); err != nil {
		return
	}
	//开始请求
	var tryNum int64
	for tryNum = 0; tryNum <= optionBak.TryNum; tryNum++ {
		select {
		case <-obj.ctx.Done():
			obj.Close()
			return nil, errors.New("http client closed")
		case <-preCtx.Done():
			return nil, preCtx.Err()
		default:
			option := optionBak
			if option.BeforCallBack != nil {
				if err = option.BeforCallBack(preCtx, &option); err != nil {
					if errors.Is(err, ErrFatal) {
						return
					} else {
						continue
					}
				}
			}
			if err = option.optionInit(); err != nil {
				return
			}
			resp, err = obj.tempRequest(preCtx, option)
			if err != nil { //有错误
				if errors.Is(err, ErrFatal) { //致命错误直接返回
					return
				} else if option.ErrCallBack != nil && option.ErrCallBack(preCtx, err) { //不是致命错误，有错误回调,错误回调true,直接返回
					return
				}
			} else if option.AfterCallBack == nil { //没有错误，且没有回调，直接返回
				return
			} else if err = option.AfterCallBack(preCtx, resp); err != nil { //没有错误，有回调，回调错误
				if errors.Is(err, ErrFatal) { //致命错误直接返回
					return
				} else if option.ErrCallBack != nil && option.ErrCallBack(preCtx, err) { //不是致命错误，有错误回调,错误回调true,直接返回
					return
				}
			} else { //没有错误，有回调，没有回调错误，直接返回
				return
			}
		}
	}
	if err != nil { //有错误直接返回错误
		return
	}
	return resp, errors.New("max try num")
}
func verifyProxy(proxyUrl string) (*url.URL, error) {
	proxy, err := url.Parse(proxyUrl)
	if err != nil {
		return nil, err
	}
	switch proxy.Scheme {
	case "http", "socks5", "https":
		return proxy, nil
	default:
		return nil, tools.WrapError(ErrFatal, "不支持的代理协议")
	}
}
func (obj *Client) tempRequest(preCtx context.Context, option RequestOption) (response *Response, err error) {
	method := strings.ToUpper(option.Method)
	href := option.converUrl
	var reqs *http.Request
	//构造ctxData
	ctxData := new(reqCtxData)
	ctxData.disProxy = option.DisProxy
	if option.Proxy != "" { //代理相关构造
		tempProxy, err := verifyProxy(option.Proxy)
		if err != nil {
			return response, tools.WrapError(ErrFatal, err)
		}
		ctxData.proxy = tempProxy
	} else if tempProxy := obj.dialer.Proxy(); tempProxy != nil {
		ctxData.proxy = tempProxy
	}
	if option.RedirectNum != 0 { //重定向次数
		ctxData.redirectNum = option.RedirectNum
	}
	//构造ctx,cnl
	var cancel context.CancelFunc
	var reqCtx context.Context
	if option.Timeout > 0 { //超时
		reqCtx, cancel = context.WithTimeout(context.WithValue(preCtx, keyPrincipalID, ctxData), time.Duration(option.Timeout)*time.Second)
	} else {
		reqCtx, cancel = context.WithCancel(context.WithValue(preCtx, keyPrincipalID, ctxData))
	}
	defer func() {
		if err != nil {
			cancel()
			if response != nil {
				response.Close()
			}
		}
	}()
	//创建request
	if option.body != nil {
		reqs, err = http.NewRequestWithContext(reqCtx, method, href, option.body)
	} else {
		reqs, err = http.NewRequestWithContext(reqCtx, method, href, nil)
	}
	if err != nil {
		return response, tools.WrapError(ErrFatal, err)
	}
	ctxData.url = reqs.URL
	ctxData.host = reqs.Host
	if reqs.URL.Scheme == "file" {
		filePath := re.Sub(`^/+`, "", reqs.URL.Path)
		fileContent, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		cancel()
		return &Response{
			content:  fileContent,
			filePath: filePath,
		}, nil
	}
	//判断ws
	switch reqs.URL.Scheme {
	case "ws":
		ctxData.ws = true
		reqs.URL.Scheme = "http"
	case "wss":
		ctxData.ws = true
		reqs.URL.Scheme = "https"
	}
	//添加headers
	var headOk bool
	if reqs.Header, headOk = option.Headers.(http.Header); !headOk {
		return response, tools.WrapError(ErrFatal, "headers 转换错误")
	}

	if reqs.Header.Get("Content-Type") == "" && reqs.Header.Get("content-type") == "" && option.ContentType != "" {
		reqs.Header.Set("Content-Type", option.ContentType)
	}
	//host构造
	if option.Host != "" {
		reqs.Host = option.Host
	} else if reqs.Header.Get("Host") != "" {
		reqs.Host = reqs.Header.Get("Host")
	}
	//添加cookies
	if option.Cookies != nil {
		cooks, cookOk := option.Cookies.(Cookies)
		if !cookOk {
			return response, tools.WrapError(ErrFatal, "cookies 转换错误")
		}
		for _, vv := range cooks {
			reqs.AddCookie(vv)
		}
	}
	//开始发送请求
	var r *http.Response
	var err2 error
	if ctxData.ws {
		websocket.SetClientHeaders(reqs.Header, option.WsOption)
	}
	// ja3相关处理
	if !ctxData.disProxy && ctxData.proxy != nil { //修改host,addr
		rawPort := reqs.URL.Port()
		if rawPort == "" {
			if reqs.URL.Scheme == "https" {
				rawPort = "443"
			} else {
				rawPort = "80"
			}
		}
		rawAddr := net.JoinHostPort(reqs.URL.Hostname(), rawPort)
		ctxData.rawMd5 = tools.Hex(tools.Md5(fmt.Sprintf("%s::%s", ctxData.proxy.Hostname(), rawAddr)))
		ctxData.rawAddr, ctxData.rawHost, reqs.URL.Host = rawAddr, reqs.URL.Host, ctxData.rawMd5
	}
	r, err = obj.getClient(option).Do(reqs)
	if r != nil {
		if ctxData.ws {
			if r.StatusCode == 101 {
				option.DisRead = true
			} else if err == nil {
				err = errors.New("statusCode not 101")
			}
		} else if r.Header.Get("Content-Type") == "text/event-stream" {
			option.DisRead = true
		}
		if response, err2 = obj.newResponse(reqCtx, cancel, r, option); err2 != nil { //创建 response
			return response, err2
		}
		if ctxData.ws && r.StatusCode == 101 {
			if response.webSocket, err2 = websocket.NewClientConn(r); err2 != nil { //创建 websocket
				return response, err2
			}
		}
	}
	return response, err
}
