package downloader

import (
	"context"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nspcc-dev/neofs-api-go/pkg/client"
	"github.com/nspcc-dev/neofs-api-go/pkg/container"
	"github.com/nspcc-dev/neofs-api-go/pkg/object"
	"github.com/nspcc-dev/neofs-api-go/pkg/token"
	"github.com/nspcc-dev/neofs-http-gate/neofs"
	"github.com/nspcc-dev/neofs-http-gate/tokens"
	"github.com/pkg/errors"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	getOptionsPool = sync.Pool{
		New: func() interface{} {
			return new(neofs.GetOptions)
		},
	}

	searchOptionsPool = sync.Pool{
		New: func() interface{} {
			return new(neofs.SearchOptions)
		},
	}
)

type (
	detector struct {
		io.Writer
		sync.Once
		contentType string
	}

	request struct {
		*fasthttp.RequestCtx
		log          *zap.Logger
		objectClient neofs.ObjectClient
	}

	objectIDs []*object.ID
)

func newDetector(w io.Writer) *detector {
	return &detector{Writer: w}
}

func (d *detector) Write(data []byte) (int, error) {
	d.Once.Do(func() {
		d.contentType = http.DetectContentType(data)
	})
	return d.Writer.Write(data)
}

func (r *request) receiveFile(options *neofs.GetOptions) {
	var (
		err      error
		dis      = "inline"
		start    = time.Now()
		filename string
	)
	if err = tokens.StoreBearerToken(r.RequestCtx); err != nil {
		r.log.Error("could not fetch and store bearer token", zap.Error(err))
		r.Error("could not fetch and store bearer token", fasthttp.StatusBadRequest)
		return
	}
	writer := newDetector(r.Response.BodyWriter())
	options.Writer = writer
	obj, err := r.objectClient.Get(r.RequestCtx, options)
	if err != nil {
		r.log.Error(
			"could not receive object",
			zap.Stringer("elapsed", time.Since(start)),
			zap.Error(err),
		)
		var (
			msg  = errors.Wrap(err, "could not receive object").Error()
			code = fasthttp.StatusBadRequest
		)
		if st, ok := status.FromError(errors.Cause(err)); ok && st != nil {
			if st.Code() == codes.NotFound {
				code = fasthttp.StatusNotFound
			}
			msg = st.Message()
		}
		r.Error(msg, code)
		return
	}
	if r.Request.URI().QueryArgs().GetBool("download") {
		dis = "attachment"
	}
	r.Response.Header.Set("Content-Length", strconv.FormatUint(obj.PayloadSize(), 10))
	r.Response.Header.Set("x-object-id", obj.ID().String())
	r.Response.Header.Set("x-owner-id", obj.OwnerID().String())
	r.Response.Header.Set("x-container-id", obj.ContainerID().String())
	for _, attr := range obj.Attributes() {
		key := attr.Key()
		val := attr.Value()
		r.Response.Header.Set("x-"+key, val)
		switch key {
		case object.AttributeFileName:
			filename = val
		case object.AttributeTimestamp:
			value, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				r.log.Info("couldn't parse creation date",
					zap.String("key", key),
					zap.String("val", val),
					zap.Error(err))
				continue
			}
			r.Response.Header.Set("Last-Modified",
				time.Unix(value, 0).Format(time.RFC1123))
		}
	}
	r.SetContentType(writer.contentType)
	r.Response.Header.Set("Content-Disposition", dis+"; filename="+path.Base(filename))
}

func (o objectIDs) Slice() []string {
	res := make([]string, 0, len(o))
	for _, oid := range o {
		res = append(res, oid.String())
	}
	return res
}

type Downloader struct {
	log           *zap.Logger
	plant         neofs.ClientPlant
	getOperations struct {
		client       client.Client
		sessionToken *token.SessionToken
	}
}

func New(ctx context.Context, log *zap.Logger, plant neofs.ClientPlant) (*Downloader, error) {
	var err error
	d := &Downloader{log: log, plant: plant}
	d.getOperations.client, d.getOperations.sessionToken, err = d.plant.GetReusableArtifacts(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get neofs client's reusable artifacts")
	}
	return d, nil
}

func (a *Downloader) newRequest(ctx *fasthttp.RequestCtx, log *zap.Logger) *request {
	return &request{
		RequestCtx:   ctx,
		log:          log,
		objectClient: a.plant.Object(),
	}
}

func (a *Downloader) DownloadByAddress(c *fasthttp.RequestCtx) {
	var (
		err     error
		address = object.NewAddress()
		cid, _  = c.UserValue("cid").(string)
		oid, _  = c.UserValue("oid").(string)
		val     = strings.Join([]string{cid, oid}, "/")
		log     = a.log.With(zap.String("cid", cid), zap.String("oid", oid))
	)
	if err = address.Parse(val); err != nil {
		log.Error("wrong object address", zap.Error(err))
		c.Error("wrong object address", fasthttp.StatusBadRequest)
		return
	}
	getOpts := getOptionsPool.Get().(*neofs.GetOptions)
	defer getOptionsPool.Put(getOpts)
	getOpts.Client = a.getOperations.client
	getOpts.SessionToken = a.getOperations.sessionToken
	getOpts.ObjectAddress = address
	getOpts.Writer = nil
	a.newRequest(c, log).receiveFile(getOpts)
}

func (a *Downloader) DownloadByAttribute(c *fasthttp.RequestCtx) {
	var (
		err     error
		scid, _ = c.UserValue("cid").(string)
		key, _  = c.UserValue("attr_key").(string)
		val, _  = c.UserValue("attr_val").(string)
		log     = a.log.With(zap.String("cid", scid), zap.String("attr_key", key), zap.String("attr_val", val))
	)
	cid := container.NewID()
	if err = cid.Parse(scid); err != nil {
		log.Error("wrong container id", zap.Error(err))
		c.Error("wrong container id", fasthttp.StatusBadRequest)
		return
	}
	searchOpts := searchOptionsPool.Get().(*neofs.SearchOptions)
	defer searchOptionsPool.Put(searchOpts)
	searchOpts.Client = a.getOperations.client
	searchOpts.SessionToken = a.getOperations.sessionToken
	searchOpts.BearerToken = nil
	searchOpts.ContainerID = cid
	searchOpts.Attribute.Key = key
	searchOpts.Attribute.Value = val
	var ids []*object.ID
	if ids, err = a.plant.Object().Search(c, searchOpts); err != nil {
		log.Error("something went wrong", zap.Error(err))
		c.Error("something went wrong", fasthttp.StatusBadRequest)
		return
	} else if len(ids) == 0 {
		log.Debug("object not found")
		c.Error("object not found", fasthttp.StatusNotFound)
		return
	}
	if len(ids) > 1 {
		log.Debug("found multiple objects",
			zap.Strings("object_ids", objectIDs(ids).Slice()),
			zap.Stringer("show_object_id", ids[0]))
	}
	address := object.NewAddress()
	address.SetContainerID(cid)
	address.SetObjectID(ids[0])
	getOpts := getOptionsPool.Get().(*neofs.GetOptions)
	defer getOptionsPool.Put(getOpts)
	getOpts.Client = a.getOperations.client
	getOpts.SessionToken = a.getOperations.sessionToken
	getOpts.ObjectAddress = address
	getOpts.Writer = nil
	a.newRequest(c, log).receiveFile(getOpts)
}