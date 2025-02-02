package uploader

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/nspcc-dev/neofs-api-go/pkg/container"
	"github.com/nspcc-dev/neofs-api-go/pkg/object"
	"github.com/nspcc-dev/neofs-api-go/pkg/owner"
	"github.com/nspcc-dev/neofs-api-go/pkg/token"
	"github.com/nspcc-dev/neofs-http-gate/neofs"
	"github.com/nspcc-dev/neofs-http-gate/tokens"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

const (
	jsonHeader   = "application/json; charset=UTF-8"
	drainBufSize = 4096
)

var putOptionsPool = sync.Pool{
	New: func() interface{} {
		return new(neofs.PutOptions)
	},
}

type Uploader struct {
	log                    *zap.Logger
	plant                  neofs.ClientPlant
	enableDefaultTimestamp bool
}

func New(log *zap.Logger, plant neofs.ClientPlant, enableDefaultTimestamp bool) *Uploader {
	return &Uploader{log, plant, enableDefaultTimestamp}
}

func (u *Uploader) Upload(c *fasthttp.RequestCtx) {
	var (
		err        error
		file       MultipartFile
		addr       *object.Address
		cid        = container.NewID()
		scid, _    = c.UserValue("cid").(string)
		log        = u.log.With(zap.String("cid", scid))
		bodyStream = c.RequestBodyStream()
		drainBuf   = make([]byte, drainBufSize)
	)
	if err = tokens.StoreBearerToken(c); err != nil {
		log.Error("could not fetch bearer token", zap.Error(err))
		c.Error("could not fetch bearer token", fasthttp.StatusBadRequest)
		return
	}
	if err = cid.Parse(scid); err != nil {
		log.Error("wrong container id", zap.Error(err))
		c.Error("wrong container id", fasthttp.StatusBadRequest)
		return
	}
	defer func() {
		// If the temporary reader can be closed - let's close it.
		if file == nil {
			return
		}
		err := file.Close()
		log.Debug(
			"close temporary multipart/form file",
			zap.Stringer("address", addr),
			zap.String("filename", file.FileName()),
			zap.Error(err),
		)
	}()
	boundary := string(c.Request.Header.MultipartFormBoundary())
	if file, err = fetchMultipartFile(u.log, bodyStream, boundary); err != nil {
		log.Error("could not receive multipart/form", zap.Error(err))
		c.Error("could not receive multipart/form: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}
	filtered := filterHeaders(u.log, &c.Request.Header)
	attributes := make([]*object.Attribute, 0, len(filtered))
	// prepares attributes from filtered headers
	for key, val := range filtered {
		attribute := object.NewAttribute()
		attribute.SetKey(key)
		attribute.SetValue(val)
		attributes = append(attributes, attribute)
	}
	// sets FileName attribute if it wasn't set from header
	if _, ok := filtered[object.AttributeFileName]; !ok {
		filename := object.NewAttribute()
		filename.SetKey(object.AttributeFileName)
		filename.SetValue(file.FileName())
		attributes = append(attributes, filename)
	}
	// sets Timestamp attribute if it wasn't set from header and enabled by settings
	if _, ok := filtered[object.AttributeTimestamp]; !ok && u.enableDefaultTimestamp {
		timestamp := object.NewAttribute()
		timestamp.SetKey(object.AttributeTimestamp)
		timestamp.SetValue(strconv.FormatInt(time.Now().Unix(), 10))
		attributes = append(attributes, timestamp)
	}
	oid, bt := u.fetchOwnerAndBearerToken(c)
	putOpts := putOptionsPool.Get().(*neofs.PutOptions)
	defer putOptionsPool.Put(putOpts)
	// Try to put file into NeoFS or throw an error.
	putOpts.Client, putOpts.SessionToken, err = u.plant.ConnectionArtifacts()
	if err != nil {
		log.Error("failed to get neofs connection artifacts", zap.Error(err))
		c.Error("failed to get neofs connection artifacts", fasthttp.StatusInternalServerError)
		return
	}
	putOpts.Attributes = attributes
	putOpts.BearerToken = bt
	putOpts.ContainerID = cid
	putOpts.OwnerID = oid
	putOpts.Reader = file
	if addr, err = u.plant.Object().Put(c, putOpts); err != nil {
		log.Error("could not store file in neofs", zap.Error(err))
		c.Error("could not store file in neofs", fasthttp.StatusBadRequest)
		return
	}
	// Try to return the response, otherwise, if something went wrong, throw an error.
	if err = newPutResponse(addr).encode(c); err != nil {
		log.Error("could not prepare response", zap.Error(err))
		c.Error("could not prepare response", fasthttp.StatusBadRequest)

		return
	}
	// Multipart is multipart and thus can contain more than one part which
	// we ignore at the moment. Also, when dealing with chunked encoding
	// the last zero-length chunk might be left unread (because multipart
	// reader only cares about its boundary and doesn't look further) and
	// it will be (erroneously) interpreted as the start of the next
	// pipelined header. Thus we need to drain the body buffer.
	for {
		_, err = bodyStream.Read(drainBuf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
	}
	// Report status code and content type.
	c.Response.SetStatusCode(fasthttp.StatusOK)
	c.Response.Header.SetContentType(jsonHeader)
}

func (u *Uploader) fetchOwnerAndBearerToken(ctx context.Context) (*owner.ID, *token.BearerToken) {
	if token, err := tokens.LoadBearerToken(ctx); err == nil && token != nil {
		return token.Issuer(), token
	}
	return u.plant.OwnerID(), nil
}

type putResponse struct {
	ObjectID    string `json:"object_id"`
	ContainerID string `json:"container_id"`
}

func newPutResponse(addr *object.Address) *putResponse {
	return &putResponse{
		ObjectID:    addr.ObjectID().String(),
		ContainerID: addr.ContainerID().String(),
	}
}

func (pr *putResponse) encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "\t")
	return enc.Encode(pr)
}
