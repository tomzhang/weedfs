package weed_server

import (
	"bytes"
	"code.google.com/p/weed-fs/go/glog"
	"code.google.com/p/weed-fs/go/operation"
	"code.google.com/p/weed-fs/go/stats"
	"code.google.com/p/weed-fs/go/storage"
	"code.google.com/p/weed-fs/go/topology"
	"github.com/disintegration/imaging"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var fileNameEscaper = strings.NewReplacer("\\", "\\\\", "\"", "\\\"")

func (vs *VolumeServer) storeHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		stats.ReadRequest()
		vs.GetOrHeadHandler(w, r, true)
	case "HEAD":
		stats.ReadRequest()
		vs.GetOrHeadHandler(w, r, false)
	case "DELETE":
		stats.DeleteRequest()
		secure(vs.whiteList, vs.DeleteHandler)(w, r)
	case "PUT":
		stats.WriteRequest()
		secure(vs.whiteList, vs.PostHandler)(w, r)
	case "POST":
		stats.WriteRequest()
		secure(vs.whiteList, vs.PostHandler)(w, r)
	}
}

func (vs *VolumeServer) GetOrHeadHandler(w http.ResponseWriter, r *http.Request, isGetMethod bool) {
	n := new(storage.Needle)
	vid, fid, filename, ext, _ := parseURLPath(r.URL.Path)
	volumeId, err := storage.NewVolumeId(vid)
	if err != nil {
		glog.V(2).Infoln("parsing error:", err, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	err = n.ParsePath(fid)
	if err != nil {
		glog.V(2).Infoln("parsing fid error:", err, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	glog.V(4).Infoln("volume", volumeId, "reading", n)
	if !vs.store.HasVolume(volumeId) {
		lookupResult, err := operation.Lookup(vs.masterNode, volumeId.String())
		glog.V(2).Infoln("volume", volumeId, "found on", lookupResult, "error", err)
		if err == nil && len(lookupResult.Locations) > 0 {
			http.Redirect(w, r, "http://"+lookupResult.Locations[0].PublicUrl+r.URL.Path, http.StatusMovedPermanently)
		} else {
			glog.V(2).Infoln("lookup error:", err, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	cookie := n.Cookie
	count, e := vs.store.Read(volumeId, n)
	glog.V(4).Infoln("read bytes", count, "error", e)
	if e != nil || count <= 0 {
		glog.V(0).Infoln("read error:", e, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if n.Cookie != cookie {
		glog.V(0).Infoln("request", r.URL.Path, "with unmaching cookie seen:", cookie, "expected:", n.Cookie, "from", r.RemoteAddr, "agent", r.UserAgent())
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if n.LastModified != 0 {
		w.Header().Set("Last-Modified", time.Unix(int64(n.LastModified), 0).UTC().Format(http.TimeFormat))
		if r.Header.Get("If-Modified-Since") != "" {
			if t, parseError := time.Parse(http.TimeFormat, r.Header.Get("If-Modified-Since")); parseError == nil {
				if t.Unix() >= int64(n.LastModified) {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
	}
	if n.NameSize > 0 && filename == "" {
		filename = string(n.Name)
		dotIndex := strings.LastIndex(filename, ".")
		if dotIndex > 0 {
			ext = filename[dotIndex:]
		}
	}
	mtype := ""
	if ext != "" {
		mtype = mime.TypeByExtension(ext)
	}
	if n.MimeSize > 0 {
		mtype = string(n.Mime)
	}
	if mtype != "" {
		w.Header().Set("Content-Type", mtype)
	}
	if filename != "" {
		w.Header().Set("Content-Disposition", "filename="+fileNameEscaper.Replace(filename))
	}
	if ext != ".gz" {
		if n.IsGzipped() {
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				w.Header().Set("Content-Encoding", "gzip")
			} else {
				if n.Data, err = storage.UnGzipData(n.Data); err != nil {
					glog.V(0).Infoln("lookup error:", err, r.URL.Path)
				}
			}
		}
	}
	if ext == ".png" || ext == ".jpg" || ext == ".gif" {
		if srcImage, _, err := image.Decode(bytes.NewReader(n.Data)); err == nil {
			width, height := 0, 0
			if r.FormValue("width") != "" {
				width, _ = strconv.Atoi(r.FormValue("width"))
			}
			if r.FormValue("height") != "" {
				height, _ = strconv.Atoi(r.FormValue("height"))
			}
			if width != 0 || height != 0 {
				bounds := srcImage.Bounds()
				var dstImage *image.NRGBA
				if width == height && bounds.Dx() != bounds.Dy() {
					dstImage = imaging.Thumbnail(srcImage, width, height, imaging.Lanczos)
				} else {
					dstImage = imaging.Resize(srcImage, width, height, imaging.Lanczos)
				}
				var buf bytes.Buffer
				switch ext {
				case ".png":
					png.Encode(&buf, dstImage)
				case ".jpg":
					jpeg.Encode(&buf, dstImage, nil)
				case ".gif":
					gif.Encode(&buf, dstImage, nil)
				}
				n.Data = buf.Bytes()
			}
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(n.Data)))
	if isGetMethod {
		if _, e = w.Write(n.Data); e != nil {
			glog.V(0).Infoln("response write error:", e)
		}
	}
}

func (vs *VolumeServer) PostHandler(w http.ResponseWriter, r *http.Request) {
	if e := r.ParseForm(); e != nil {
		glog.V(0).Infoln("form parse error:", e)
		writeJsonError(w, r, e)
		return
	}
	vid, _, _, _, _ := parseURLPath(r.URL.Path)
	volumeId, ve := storage.NewVolumeId(vid)
	if ve != nil {
		glog.V(0).Infoln("NewVolumeId error:", ve)
		writeJsonError(w, r, ve)
		return
	}
	needle, ne := storage.NewNeedle(r)
	if ne != nil {
		writeJsonError(w, r, ne)
		return
	}

	ret := operation.UploadResult{}
	size, errorStatus := topology.ReplicatedWrite(vs.masterNode, vs.store, volumeId, needle, r)
	if errorStatus == "" {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
		ret.Error = errorStatus
	}
	if needle.HasName() {
		ret.Name = string(needle.Name)
	}
	ret.Size = size
	writeJsonQuiet(w, r, ret)
}

func (vs *VolumeServer) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	n := new(storage.Needle)
	vid, fid, _, _, _ := parseURLPath(r.URL.Path)
	volumeId, _ := storage.NewVolumeId(vid)
	n.ParsePath(fid)

	glog.V(2).Infoln("deleting", n)

	cookie := n.Cookie
	count, ok := vs.store.Read(volumeId, n)

	if ok != nil {
		m := make(map[string]uint32)
		m["size"] = 0
		writeJsonQuiet(w, r, m)
		return
	}

	if n.Cookie != cookie {
		glog.V(0).Infoln("delete", r.URL.Path, "with unmaching cookie from ", r.RemoteAddr, "agent", r.UserAgent())
		return
	}

	n.Size = 0
	ret := topology.ReplicatedDelete(vs.masterNode, vs.store, volumeId, n, r)

	if ret != 0 {
		w.WriteHeader(http.StatusAccepted)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}

	m := make(map[string]uint32)
	m["size"] = uint32(count)
	writeJsonQuiet(w, r, m)
}

//Experts only: takes multiple fid parameters. This function does not propagate deletes to replicas.
func (vs *VolumeServer) batchDeleteHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var ret []operation.DeleteResult
	for _, fid := range r.Form["fid"] {
		vid, id_cookie, err := operation.ParseFileId(fid)
		if err != nil {
			ret = append(ret, operation.DeleteResult{Fid: fid, Error: err.Error()})
			continue
		}
		n := new(storage.Needle)
		volumeId, _ := storage.NewVolumeId(vid)
		n.ParsePath(id_cookie)
		glog.V(4).Infoln("batch deleting", n)
		cookie := n.Cookie
		if _, err := vs.store.Read(volumeId, n); err != nil {
			ret = append(ret, operation.DeleteResult{Fid: fid, Error: err.Error()})
			continue
		}
		if n.Cookie != cookie {
			ret = append(ret, operation.DeleteResult{Fid: fid, Error: "File Random Cookie does not match."})
			glog.V(0).Infoln("deleting", fid, "with unmaching cookie from ", r.RemoteAddr, "agent", r.UserAgent())
			return
		}
		if size, err := vs.store.Delete(volumeId, n); err != nil {
			ret = append(ret, operation.DeleteResult{Fid: fid, Error: err.Error()})
		} else {
			ret = append(ret, operation.DeleteResult{Fid: fid, Size: int(size)})
		}
	}

	w.WriteHeader(http.StatusAccepted)

	writeJsonQuiet(w, r, ret)
}
