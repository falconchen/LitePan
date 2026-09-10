package pan115open

import (
	"encoding/json"
	"strconv"
	"strings"

	"litepan/pkg/jsonvalue"
)

type flexString = jsonvalue.FlexibleString

type flexNumber string

func (f *flexNumber) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexNumber(strings.TrimSpace(s))
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexNumber(n.String())
	return nil
}

func (f flexNumber) String() string { return strings.TrimSpace(string(f)) }

func (f flexNumber) int64() int64 {
	s := f.String()
	if s == "" || s == "0" {
		return 0
	}
	if v, err := json.Number(s).Int64(); err == nil {
		return v
	}
	return 0
}

type Addition struct {
	AccessToken  string     `json:"access_token" label:"访问令牌 access_token" type:"password" form:"required,pair=auth"`
	RefreshToken string     `json:"refresh_token" label:"刷新令牌 refresh_token" type:"password" form:"required,pair=auth"`
	DownloadMode string     `json:"download_mode" label:"下载模式" type:"select" options:"redirect:302重定向,proxy:本机代理" default:"redirect" form:"pair=opts2"`
	DeleteMode   string     `json:"delete_mode" label:"删除模式" type:"select" options:"trash:移到回收站,delete:永久删除" default:"trash" form:"pair=opts2"`
	RootFolderID string     `json:"root_folder_id" label:"根目录ID（默认 0）" default:"0" form:"pair=opts1"`
	CacheTTL     flexString `json:"cache_ttl" label:"缓存时间(分钟)" type:"number" default:"30" form:"pair=opts1"`

	SinglePartLimitMB flexString `json:"single_part_limit_mb" label:"分片阈值（MB，超过则分片上传）" type:"number" default:"10" form:"pair=upload"`
	UploadPartSizeMB  flexString `json:"upload_part_size_mb" label:"分片大小（MB）" type:"number" default:"5" form:"pair=upload"`
}

// 上传相关配置的默认值与取值范围。留空或填非法值时回落到默认值，
// 超出范围则夹到边界，避免用户填出 OSS 不接受的参数。
const (
	mib = int64(1024 * 1024)

	defaultSinglePartLimitMB = 10
	minSinglePartLimitMB     = 1
	maxSinglePartLimitMB     = 5 * 1024 // OSS 单次 PutObject 上限 5 GiB

	defaultUploadPartSizeMB = 5
	minUploadPartSizeMB     = 1        // OSS 要求非最后一片不小于 100 KiB，这里取更保守的 1 MiB
	maxUploadPartSizeMB     = 5 * 1024 // OSS 单个分片上限 5 GiB
)

func positiveIntOr(value flexString, def int) int {
	s := strings.TrimSpace(value.String())
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func mibOr(value flexString, def, lo, hi int) int64 {
	n := positiveIntOr(value, def)
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return int64(n) * mib
}

// singlePartLimit 返回"单次 PutObject 的最大文件大小"，超过它就走分片。
func (d *Driver) singlePartLimit() int64 {
	return mibOr(d.add.SinglePartLimitMB, defaultSinglePartLimitMB, minSinglePartLimitMB, maxSinglePartLimitMB)
}

// uploadPartSize 返回配置的分片大小。注意这只是"期望值"，
// 实际用的还要经 calculateOSSPartSize 按 10000 片上限修正。
func (d *Driver) uploadPartSize() int64 {
	return mibOr(d.add.UploadPartSizeMB, defaultUploadPartSizeMB, minUploadPartSizeMB, maxUploadPartSizeMB)
}
