module github.com/deepfabric/busybee

go 1.13

require (
	github.com/RoaringBitmap/roaring v0.4.21
	github.com/deepfabric/beehive v0.0.0-20191220073544-b4be866eb8de
	github.com/deepfabric/prophet v0.0.0-20191202055442-cec7351ee5cf
	github.com/fagongzi/goetty v1.3.2
	github.com/fagongzi/log v0.0.0-20191122063922-293b75312445
	github.com/fagongzi/util v0.0.0-20191031020235-c0f29a56724d
	github.com/gogo/protobuf v1.3.1
	github.com/labstack/echo v3.3.10+incompatible
	github.com/labstack/gommon v0.3.0 // indirect
	github.com/shirou/gopsutil v2.19.10+incompatible // indirect
	github.com/stretchr/testify v1.4.0
	golang.org/x/text v0.3.2 // indirect
)

replace github.com/coreos/etcd => github.com/deepfabric/etcd v3.3.17+incompatible
