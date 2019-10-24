package adapter

import (
	"net"
	"os"
	"sync"
	"time"

	"github.com/op/go-logging"
	"gitlab.x.lan/yunshan/droplet-libs/debug"
	"gitlab.x.lan/yunshan/droplet-libs/pool"
	"gitlab.x.lan/yunshan/droplet-libs/queue"
	"gitlab.x.lan/yunshan/droplet-libs/stats"
	. "gitlab.x.lan/yunshan/droplet-libs/utils"

	"gitlab.x.lan/yunshan/droplet/dropletctl"
)

const (
	LISTEN_PORT      = 20033
	QUEUE_BATCH_SIZE = 4096
	TRIDENT_TIMEOUT  = 2 * time.Second

	BATCH_SIZE = 128

	TRIDENT_DISPATCHER_MAX = 16
)

var log = logging.MustGetLogger("trident_adapter")

type TridentKey = uint32

type packetBuffer struct {
	buffer    []byte
	tridentIp uint32
	decoder   SequentialDecoder
	hash      uint8
}

type tridentDispatcher struct {
	cache        []*packetBuffer
	timestamp    []time.Duration // 对应cache[i]为nil时，值为后续最近一个包的timestamp，用于判断超时
	maxTimestamp time.Duration   // 历史接收包的最大时间戳，用于判断trident重启
	dropped      uint64
	seq          uint64 // cache中的第一个seq
	startIndex   uint64
}

type tridentInstance struct {
	dispatchers [TRIDENT_DISPATCHER_MAX]tridentDispatcher
}

type TridentAdapter struct {
	command
	statsCounter

	listenBufferSize int
	cacheSize        uint64

	instancesLock sync.Mutex // 仅用于droplet-ctl打印trident信息
	instances     map[TridentKey]*tridentInstance

	slaveCount uint8
	slaves     []*slave

	running  bool
	listener *net.UDPConn
}

func (p *packetBuffer) init(ip uint32) {
	p.tridentIp = ip
	p.decoder.initSequentialDecoder(p.buffer)
}

func (p *packetBuffer) calcHash() uint8 {
	hash := p.tridentIp ^ uint32(p.decoder.tridentDispatcherIndex)
	p.hash = uint8(hash>>24) ^ uint8(hash>>16) ^ uint8(hash>>8) ^ uint8(hash)
	p.hash = (p.hash >> 6) ^ (p.hash >> 4) ^ (p.hash >> 2) ^ p.hash
	return p.hash
}

func minPowerOfTwo(v uint32) uint32 {
	for i := 0; i < 30; i++ {
		if v <= 1<<i {
			return 1 << i
		}
	}
	return 1 << 30
}

func NewTridentAdapter(queues []queue.QueueWriter, listenBufferSize int, cacheSize uint32) *TridentAdapter {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{Port: LISTEN_PORT})
	if err != nil {
		log.Error(err)
		return nil
	}
	adapter := &TridentAdapter{
		listenBufferSize: listenBufferSize,
		cacheSize:        uint64(minPowerOfTwo(cacheSize)),
		slaveCount:       uint8(len(queues)),
		slaves:           make([]*slave, len(queues)),

		instances: make(map[TridentKey]*tridentInstance),
	}
	for i := uint8(0); i < adapter.slaveCount; i++ {
		adapter.slaves[i] = newSlave(int(i), queues[i])
	}
	adapter.statsCounter.init()
	adapter.listener = listener
	adapter.command.init(adapter)
	stats.RegisterCountable("trident-adapter", adapter)
	debug.Register(dropletctl.DROPLETCTL_ADAPTER, adapter)
	return adapter
}

func (a *TridentAdapter) GetStatsCounter() interface{} {
	counter := &PacketCounter{}
	masterCounter := a.statsCounter.GetStatsCounter().(*PacketCounter)
	counter.add(masterCounter)
	for i := uint8(0); i < a.slaveCount; i++ {
		slaveCounter := a.slaves[i].statsCounter.GetStatsCounter().(*PacketCounter)
		counter.add(slaveCounter)
	}
	return counter
}

func (a *TridentAdapter) GetCounter() interface{} {
	counter := a.statsCounter.GetCounter().(*PacketCounter)
	for i := uint8(0); i < a.slaveCount; i++ {
		slaveCounter := a.slaves[i].statsCounter.GetCounter().(*PacketCounter)
		counter.add(slaveCounter)
	}
	return counter
}

func (a *TridentAdapter) Closed() bool {
	return false // FIXME: never close?
}

func cacheLookup(dispatcher *tridentDispatcher, packet *packetBuffer, cacheSize uint64, slaves []*slave) (uint64, uint64) {
	decoder := &packet.decoder
	seq := decoder.Seq()
	timestamp := decoder.timestamp

	// 初始化
	if dispatcher.seq == 0 {
		dispatcher.seq = seq
		log.Infof("receive first packet from trident %v index %d, with seq %d",
			IpFromUint32(packet.tridentIp), packet.decoder.tridentDispatcherIndex, dispatcher.seq)
	}
	dropped := uint64(0)

	// 倒退
	if seq < dispatcher.seq {
		if timestamp > dispatcher.maxTimestamp { // 序列号更小但时间更大，trident重启
			log.Warningf("trident %v index %d restart but some packets lost, received timestamp %d > %d, reset sequence to max(%d-%d, %d).",
				IpFromUint32(packet.tridentIp), packet.decoder.tridentDispatcherIndex,
				timestamp, dispatcher.maxTimestamp, seq, cacheSize, 1)
			// 重启前的包如果还在cache中一定存在丢失的部分，直接抛弃且不计数。
			for i := uint64(0); i < cacheSize; i++ {
				if dispatcher.cache[i] != nil {
					releasePacketBuffer(dispatcher.cache[i])
					dispatcher.cache[i] = nil
				}
				dispatcher.timestamp[i] = 0
			}
			// 重启时不记录丢包数，因为重启的影响更大，且已经触发了告警。
			if seq > cacheSize {
				dispatcher.seq = seq - cacheSize
			} else {
				dispatcher.seq = 1
			}
			dispatcher.startIndex = 0
		} else {
			// 乱序包，丢弃并返回。注意乱序一定意味着之前已经统计到了丢包。
			// 乱序接近丢弃说明是真乱序，乱序远比丢弃小说明是真丢包。
			log.Warningf("trident %v index %d hash seq %d less than current %d, drop packet",
				IpFromUint32(packet.tridentIp), packet.decoder.tridentDispatcherIndex, seq, dispatcher.seq)
			releasePacketBuffer(packet)
			return dropped, uint64(1)
		}
	}
	if timestamp > dispatcher.maxTimestamp {
		dispatcher.maxTimestamp = timestamp
	}

	// 尽量flush直至可cache
	offset := seq - dispatcher.seq
	for i := uint64(0); i < cacheSize && offset >= cacheSize; i++ {
		if dispatcher.cache[dispatcher.startIndex] != nil {
			p := dispatcher.cache[dispatcher.startIndex]
			slaves[p.hash&uint8(len(slaves)-1)].put(p)
			dispatcher.cache[dispatcher.startIndex] = nil
		} else {
			dropped++
		}
		dispatcher.timestamp[i] = 0
		dispatcher.seq++
		dispatcher.startIndex = (dispatcher.startIndex + 1) & (cacheSize - 1)
		offset--
	}
	if offset >= cacheSize {
		gap := offset - cacheSize + 1
		dispatcher.seq += gap
		dispatcher.startIndex = (dispatcher.startIndex + gap) & (cacheSize - 1)
		dropped += uint64(gap)
		offset -= gap
	}

	// 加入cache
	current := (dispatcher.startIndex + offset) & (cacheSize - 1)
	dispatcher.cache[current] = packet
	dispatcher.timestamp[current] = timestamp
	for i := current; i != dispatcher.startIndex; { // 设置尚未到达的包的最坏timestamp
		i = (i - 1) & (cacheSize - 1)
		if dispatcher.cache[i] != nil {
			break
		}
		dispatcher.timestamp[i] = timestamp
	}

	// 尽量flush直至有残缺、或超时
	for i := uint64(0); i < cacheSize; i++ {
		if dispatcher.cache[dispatcher.startIndex] != nil { // 可以flush
			p := dispatcher.cache[dispatcher.startIndex]
			slaves[p.hash&uint8(len(slaves)-1)].put(p)
			dispatcher.cache[dispatcher.startIndex] = nil
		} else if dispatcher.timestamp[dispatcher.startIndex] == 0 { // 没有更多packet
			break
		} else if timestamp-dispatcher.timestamp[dispatcher.startIndex] > TRIDENT_TIMEOUT { // 超时
			dropped++
		} else { // 无法移动窗口
			break
		}
		dispatcher.timestamp[i] = 0

		dispatcher.seq++
		dispatcher.startIndex = (dispatcher.startIndex + 1) & (cacheSize - 1)
	}

	// 统计丢包数
	if dropped > 0 {
		dispatcher.dropped += dropped
		log.Debugf("trident %v index %d lost %d packets, packet received with seq %d, now window start with seq %d",
			IpFromUint32(packet.tridentIp), packet.decoder.tridentDispatcherIndex, dropped, seq, dispatcher.seq)
	}
	return dropped, uint64(0)
}

func (a *TridentAdapter) findAndAdd(packet *packetBuffer) {
	var dispatcher *tridentDispatcher
	instance := a.instances[packet.tridentIp]
	index := packet.decoder.tridentDispatcherIndex
	if instance == nil {
		instance = &tridentInstance{}
		dispatcher := &instance.dispatchers[index]
		dispatcher.cache = make([]*packetBuffer, a.cacheSize)
		dispatcher.timestamp = make([]time.Duration, a.cacheSize)
		a.instancesLock.Lock()
		a.instances[packet.tridentIp] = instance
		a.instancesLock.Unlock()
	}
	dispatcher = &instance.dispatchers[index]
	if dispatcher.cache == nil {
		dispatcher.cache = make([]*packetBuffer, a.cacheSize)
		dispatcher.timestamp = make([]time.Duration, a.cacheSize)
	}

	rxDropped, rxExpired := cacheLookup(dispatcher, packet, a.cacheSize, a.slaves)
	a.counter.RxPackets++
	a.counter.RxDropped += rxDropped
	a.counter.RxExpired += rxExpired
	a.stats.RxPackets++
	a.stats.RxDropped += rxDropped
	a.stats.RxExpired += rxExpired
}

var packetBufferPool = pool.NewLockFreePool(
	func() interface{} {
		packet := new(packetBuffer)
		packet.buffer = make([]byte, UDP_BUFFER_SIZE)
		return packet
	},
	pool.OptionPoolSizePerCPU(16),
	pool.OptionInitFullPoolSize(16),
)

func acquirePacketBuffer() *packetBuffer {
	return packetBufferPool.Get().(*packetBuffer)
}

func releasePacketBuffer(b *packetBuffer) {
	// 此处无初始化
	packetBufferPool.Put(b)
}

func (a *TridentAdapter) run() {
	log.Infof("Starting trident adapter Listenning <%s>", a.listener.LocalAddr())
	a.listener.SetReadDeadline(time.Now().Add(TRIDENT_TIMEOUT))
	a.listener.SetReadBuffer(a.listenBufferSize)
	batch := [BATCH_SIZE]*packetBuffer{}
	count := 0
	for a.running {
		for i := 0; i < BATCH_SIZE; i++ {
			packet := acquirePacketBuffer()
			_, remote, err := a.listener.ReadFromUDP(packet.buffer)
			if err != nil {
				if err.(net.Error).Timeout() {
					a.listener.SetReadDeadline(time.Now().Add(TRIDENT_TIMEOUT))
					break
				}
				log.Errorf("trident adapter listener.ReadFromUDP err: %s", err)
				os.Exit(1)
			}
			packet.init(IpToUint32(remote.IP.To4()))
			batch[i] = packet
			count++
		}
		for i := 0; i < count; i++ {
			if invalid := batch[i].decoder.DecodeHeader(); invalid {
				a.counter.RxErrors++
				a.stats.RxErrors++
				releasePacketBuffer(batch[i])
				continue
			}
			batch[i].calcHash()
			a.findAndAdd(batch[i])
		}
		count = 0
	}
	a.listener.Close()
	log.Info("Stopped trident adapter")
}

func (a *TridentAdapter) startSlaves() {
	for i := uint8(0); i < a.slaveCount; i++ {
		go a.slaves[i].run()
	}
}

func (a *TridentAdapter) Start() error {
	if !a.running {
		log.Info("Start trident adapter")
		a.running = true
		a.startSlaves()
		go a.run()
	}
	return nil
}
