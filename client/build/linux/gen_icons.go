//go:build ignore

// 一次性生成器：把 build/appicon.png（1024×1024）缩小为 hicolor 三个尺寸的
// 应用图标（io.github.openalc.LiteTalk.png）。纯标准库实现（面积平均缩小），
// 不引入 ImageMagick / x/image 等外部工具或依赖——产物提交进仓库，此文件仅供再生。
// 必须在本目录执行（输出为相对路径）：
//
//	cd client/build/linux && go run gen_icons.go
package main

import (
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// sizes 需要生成的 hicolor 尺寸（issue #6：256/48/32）。
var sizes = []int{256, 48, 32}

const iconID = "io.github.openalc.LiteTalk"

func main() {
	// 输出是相对路径：在错误目录执行会把图标写飞（同 tray/gen_icon.go 的护栏）。
	if _, err := os.Stat("io.github.openalc.LiteTalk.desktop"); err != nil {
		log.Fatal("请在 client/build/linux 目录执行：cd client/build/linux && go run gen_icons.go")
	}
	src, err := os.Open(filepath.Join("..", "appicon.png"))
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()
	img, err := png.Decode(src)
	if err != nil {
		log.Fatal(err)
	}
	for _, size := range sizes {
		out := downscale(img, size)
		dst := filepath.Join("icons", "hicolor", strconv.Itoa(size)+"x"+strconv.Itoa(size), "apps", iconID+".png")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			log.Fatal(err)
		}
		f, err := os.Create(dst)
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, out); err != nil {
			f.Close()
			log.Fatal(err)
		}
		f.Close()
		log.Printf("%s written (%dx%d)", dst, size, size)
	}
}

// downscale 面积平均缩小到 size×size（只缩小不放大：图标源 1024 一定够）。
// 每个目标像素按与源像素的重叠面积加权平均——非整数倍（1024→48）时
// 比最近邻干净得多，比双线性少振铃，且只需标准库。
func downscale(src image.Image, size int) *image.NRGBA {
	b := src.Bounds()
	sw, sh := float64(b.Dx()), float64(b.Dy())
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		y0 := float64(y) * sh / float64(size)
		y1 := (float64(y) + 1) * sh / float64(size)
		for x := 0; x < size; x++ {
			x0 := float64(x) * sw / float64(size)
			x1 := (float64(x) + 1) * sw / float64(size)
			var r, g, bl, a, wsum float64
			for sy := int(y0); sy < int(y1)+1 && sy < b.Dy(); sy++ {
				for sx := int(x0); sx < int(x1)+1 && sx < b.Dx(); sx++ {
					w := overlap(x0, x1, float64(sx), float64(sx)+1) *
						overlap(y0, y1, float64(sy), float64(sy)+1)
					if w <= 0 {
						continue
					}
					pr, pg, pb, pa := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r += float64(pr) * w
					g += float64(pg) * w
					bl += float64(pb) * w
					a += float64(pa) * w
					wsum += w
				}
			}
			if wsum <= 0 {
				continue // 源区为空：保持透明黑
			}
			// RGBA() 返回 16 位色（0–65535）：除 256 等价取高字节。
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / wsum / 256)
			dst.Pix[i+1] = uint8(g / wsum / 256)
			dst.Pix[i+2] = uint8(bl / wsum / 256)
			dst.Pix[i+3] = uint8(a / wsum / 256)
		}
	}
	return dst
}

// overlap 两区间 [a0,a1)、[b0,b1) 的重叠长度。
func overlap(a0, a1, b0, b1 float64) float64 {
	lo, hi := a0, a1
	if b0 > lo {
		lo = b0
	}
	if b1 < hi {
		hi = b1
	}
	return hi - lo
}
