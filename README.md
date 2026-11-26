# 多人载具席位占用与驾驶权交接

## 题面

本周三上线的双人载具在跨服战里连续出事：一名玩家在移动中掉线，车没有停，而是继续无人驾驶往前滑；队友接管之后，掉线玩家重连回来发现自己又坐回了驾驶位，把接管的人挤了出去；还有一次载具被摧毁，同一名乘客收到了两次下车通知，结算时扣了两份武器损耗。三段录屏都发到了运营群，制作人要求这周内把席位收干净。落点是模块 gsb/batch50-go-game-a-02 的 vehicle_boarding_guard.go，类型 VehicleBoardingGuard，公开接口保持不变：NewVehicleBoardingGuard(limits Limits) *VehicleBoardingGuard、Spawn(vehicleID string, seats []SeatSpec) error、Board(vehicleID, playerID string, seatIndex int) (Boarding, error)、Leave(vehicleID, playerID string) error、TransferDriver(vehicleID, fromPlayer, toPlayer string) error、ForceDismount(vehicleID string, reason DismountReason) ([]string, error)、SeatOf(vehicleID, playerID string) (SeatState, error)、Occupants(vehicleID string) []SeatState、Events(vehicleID string) []DismountEvent、GlideState(vehicleID string) (GlideInfo, error)、Tick(now time.Time) ([]DismountEvent, error)、Reconnect(vehicleID, playerID string) (Boarding, error)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，同一辆载具重复生成报 ErrVehicleExists；席位索引重复或为负、角色不是驾驶/炮手/乘客之一、驾驶位多于一个，都报 ErrBadSeat；一个驾驶位都没有报 ErrNoDriverSeat；席位总数超过 MaxSeats 报 ErrSeatLimit。第二，进车位越界报 ErrBadSeat，已经有人坐报 ErrSeatTaken，载具不存在报 ErrVehicleNotFound，选手号为空报 ErrBadPlayer。第三，乘客上限只统计乘客席位，炮手与驾驶不占额度，超了报 ErrPassengerLimit。第四，主动下车释放席位并立即可被别人占用，没上车的人下车报 ErrNotOnBoard。第五，驾驶权交接要求发起人当前确实是驾驶员，否则报 ErrNotDriver；接收人必须在本车，否则报 ErrNotOnBoard；交接成功后两人席位互换，驾驶位仍然只有一个。第六，摧毁载具一次性清空全部席位，按席位索引升序返回被清下车的名单，每个被强制下车的人写一条 DismountEvent，主动下车不写事件。第七，驾驶位空出且车上还有其他人时进入滑行宽限，GlideState 要报出是否在滑行态以及是谁留下的窗口；宽限从驾驶位空出的服务端时刻起算，Tick 传入超过 GlideSeconds 的时间点时，车上剩余人员全部按 glide_timeout 强制下车。第八，Boarding 与 DismountEvent 的序号全局单调递增，同一辆车的事件按发生顺序追加。第九，同一席位被多路并发争抢时只能有一个成功，其余必须拿到 ErrSeatTaken，不允许出现两个人都在车里；SeatOf 对不在车上的人报 ErrNotOnBoard。

三处边界必须一起处理。驾驶员在移动中掉线先进滑行态，别的玩家接管驾驶之后，原驾驶员重连只能落到乘客位，不能把驾驶权抢回去。载具摧毁和玩家自己下车同时发生时，每名乘客只应收到一次下车事件，重复事件不能二次结算。乘客断线重连要回到原席位，原席位已经被别人占了就进观战位，不能把当前占位者顶掉。

硬约束两条。一是并发：同车的上下车、驾驶权交接与摧毁事件必须串行结算，席位表在任一时刻都不能出现两个占用者或两个驾驶员。二是重启：快照要带上载具席位表、滑行窗口、事件日志与序号，恢复之后 SeatOf、Occupants、Events 与下一次进车的序号必须和重启前一致；损坏载荷在 Restore 时报 ErrBadSnapshot，且不能动到现网席位。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-02`
- 实现文件：`vehicle_boarding_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`VehicleBoardingGuard`
