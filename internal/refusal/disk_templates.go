package refusal

// ExistingDiskAttachUnsupported is returned when the user asks to mount an
// already-existing data disk. The current supported mutation is only creating
// a new data disk and attaching it to an instance.
const ExistingDiskAttachUnsupported = "当前不支持挂载已有盘。你可以让我为实例新建数据盘，例如：给 uhost-xxx 加一块 30GB 数据盘。扩已有盘属于另一类操作，需要指定原来的盘和目标容量，目前还没有开放成可执行流程。"
