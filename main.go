package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/JoshuaDoes/go-yggdrasil"
	"github.com/cornelk/hashmap"
	"github.com/google/uuid"
	"github.com/rain931215/custom-mc-library/bot"
	"github.com/rain931215/custom-mc-library/chat"
	"github.com/rain931215/custom-mc-library/data"
	net2 "github.com/rain931215/custom-mc-library/net"
	pk "github.com/rain931215/custom-mc-library/net/packet"
	"go.uber.org/atomic"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	maxTarget        = 50
	threshold        = 64
	packetBufferSize = 1024
)

type Entity struct {
	X, Y, Z *atomic.Float64
}

var (
	c                   = new(bot.Client)
	inventory           [46]*atomic.Int32
	entityList          *hashmap.HashMap
	loginSuccess        bool
	haveClaimPermission bool
	posX, posY, posZ    *atomic.Float64
	health              *atomic.Uint32
)

func init() {
	if len(os.Args) < 3 {
		fmt.Println("未指定帳號密碼")
		os.Exit(1)
	}
	posX = atomic.NewFloat64(0)
	posY = atomic.NewFloat64(0)
	posZ = atomic.NewFloat64(0)
	health = atomic.NewUint32(0)
	for i, j := 0, len(inventory); i < j; i++ {
		inventory[i] = atomic.NewInt32(0)
	}
}

func main() {
	for {
		posX.Store(0)
		posY.Store(0)
		posZ.Store(0)
		health.Store(0)
		for i, j := 0, len(inventory); i < j; i++ {
			inventory[i].Store(0)
		}
		loginSuccess = false
		haveClaimPermission = true
		entityList = hashmap.New(16)

		if err := auth(os.Args[1], os.Args[2]); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

			// join to server
			if err := c.JoinServer("jp.mcfallout.net", 25565); err != nil {
				fmt.Println(fmt.Sprintf("[錯誤] 加入伺服器時發生錯誤,錯誤傾印:%v", err))
				time.Sleep(10 * time.Second) // retry in 10 seconds
				continue
			}
		fmt.Println("[資訊] 加入伺服器成功 " + c.Name + "(UUID=" + c.Auth.UUID + ")")
		// handle connection
		err := handleConn(c.Conn())
		if err != nil {
			fmt.Println(fmt.Sprintf("[錯誤] 與伺服器的連線發生錯誤,錯誤傾印:%v", err))
		} else {
			fmt.Println("[資訊] 斷線")
		}
		time.Sleep(5 * time.Second) // wait 5 seconds before reconnect to server
	}
}

// handleConn 處理與伺服器的連線
func handleConn(conn *net2.Conn) error {
	// this will cancel all tasks if cancel function get invoked
	ctx, cancel := context.WithCancel(context.Background())
	// the queue for packets that waiting to be sent
	packetOutQueue := make(chan *pk.Packet, packetBufferSize)
	// Thread: read packet
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				p, err := pk.RecvPacket(conn.Reader, true)
				if err != nil {
					cancel()
					fmt.Println(fmt.Sprintf("[錯誤] 與伺服器的連線發生錯誤,錯誤傾印:%v", err))
					return
				}
				switch p.ID {
				case data.KeepAliveClientbound:
					// send back to server directly because the data section of packet is same
					packetOutQueue <- &pk.Packet{ID: data.KeepAliveServerbound, Data: p.Data}
				default:
					err := handlePacket(p, packetOutQueue)
					if err != nil {
						cancel()
						fmt.Println(fmt.Sprintf("[錯誤] 與伺服器的連線發生錯誤,錯誤傾印:%v", err))
						return
					}
				}
			}
		}
	}()
	// Thread: control bot
	go func() {
		gameTicker := time.NewTicker(50 * time.Millisecond)
		gameTicks := uint64(0)
		for {
			select {
			case <-ctx.Done():
				gameTicker.Stop()
				return
			case <-gameTicker.C:
				gameTicks++
				if gameTicks%13 == 0 {
					x, y, z := posX.Load(), posY.Load(), posZ.Load()
					count := 0
					for obj := range entityList.Iter() {
						entity, ok := obj.Value.(*Entity)
						if !ok {
							continue
						}
						diffX, diffY, diffZ := entity.X.Load()-x, entity.Y.Load()-y, entity.Z.Load()-z
						if diffX*diffX+diffY*diffY+diffZ*diffZ <= 36 {
							packetOutQueue <- marshal(data.UseEntity, pk.VarInt(obj.Key.(int32)), pk.VarInt(1), pk.Boolean(false))
							count++
							if count >= maxTarget {
								break
							}
						}
					}
				} else if gameTicks%200 == 0 && loginSuccess {
					if x, z := posX.Load(), posZ.Load(); x != 0 && z != 0 && x >= -100 && x <= 100 && z >= -100 && z <= 100 {
						packetOutQueue <- marshal(data.ChatMessageServerbound, pk.String("/back"))
					}
					haveClaimPermission = true
				}
				if loginSuccess && haveClaimPermission {
					breakLoop := false
					for i := 9; i <= 44; i++ {
						switch inventory[i].Load() {
						case 0, 595, 606, 903:
						default:
							inventory[i].Store(0)
							packetOutQueue <- marshal(data.ClickWindow, pk.UnsignedByte(0), pk.Short(i), pk.Byte(1), pk.Short(0), pk.VarInt(4), pk.Byte(0))
							breakLoop = true
						}
						if breakLoop {
							break
						}
					}
				}
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case v := <-packetOutQueue:
			if v == nil {
				continue
			}
			_, err := conn.Write(v.Pack(threshold))
			if err != nil {
				cancel()
				return err
			}
		}
	}
}

func handlePacket(p *pk.Packet, queue chan<- *pk.Packet) error {
	switch p.ID {
	case data.DisconnectPlay:
		var msg chat.Message
		if err := msg.Decode(bytes.NewReader(p.Data)); err != nil {
			return err
		}
		return fmt.Errorf("被伺服器中斷連線:%v", msg)
	case data.ChatMessageClientbound:
		if pos := p.Data[len(p.Data)-1]; pos == 0 || pos == 1 {
			var msg chat.Message
			if msg.Decode(bytes.NewReader(p.Data)) == nil {
				handleChat(msg.ClearString(), queue)
			}
		}
	case data.EntityRelativeMove, data.EntityLookAndRelativeMove:
		var (
			r       = bytes.NewReader(p.Data)
			eID     pk.VarInt
			x, y, z pk.Short
		)
		if eID.Decode(r) == nil && eID > 0 {
			obj, ok := entityList.Get(int32(eID))
			if ok {
				if element, ok := obj.(*Entity); ok {
					if x.Decode(r) == nil && y.Decode(r) == nil && z.Decode(r) == nil {
						element.X.Add(float64(x / 4096.0))
						element.Y.Add(float64(y / 4096.0))
						element.Z.Add(float64(z / 4096.0))
					}
				}
			}
		}
	case data.EntityTeleport:
		var (
			r       = bytes.NewReader(p.Data)
			eID     pk.VarInt
			x, y, z pk.Double
		)
		if eID.Decode(r) == nil && eID > 0 {
			obj, ok := entityList.Get(int32(eID))
			if ok {
				if element, ok := obj.(*Entity); ok {
					if x.Decode(r) == nil && y.Decode(r) == nil && z.Decode(r) == nil {
						element.X.Store(float64(x))
						element.Y.Store(float64(y))
						element.Z.Store(float64(z))
					}
				}
			}
		}
	case data.SpawnLivingEntity:
		var (
			r       = bytes.NewReader(p.Data)
			eID     pk.VarInt
			eUUID   pk.UUID
			eType   pk.VarInt
			x, y, z pk.Double
		)
		if err := eID.Decode(r); err != nil {
			return err
		}
		if eID < 0 {
			return nil
		}
		if err := eUUID.Decode(r); err != nil {
			return err
		}
		if err := eType.Decode(r); err != nil {
			return err
		}
		switch eType {
		case 22, 58, 61, 91, 93, 95:
			if x.Decode(r) == nil && y.Decode(r) == nil && z.Decode(r) == nil {
				entityList.Set(int32(eID), &Entity{X: atomic.NewFloat64(float64(x)), Y: atomic.NewFloat64(float64(y)), Z: atomic.NewFloat64(float64(z))})
			}
		}
	case data.DestroyEntities:
		var (
			r     = bytes.NewReader(p.Data)
			count pk.VarInt
		)
		if err := count.Decode(r); err != nil {
			return err
		}
		for i, j := 0, int(count); i < j; i++ {
			var entityID pk.VarInt
			if entityID.Decode(r) == nil && entityID > 0 {
				entityList.Del(int32(entityID))
			}
		}
	case data.SetSlot:
		var (
			r        = bytes.NewReader(p.Data)
			windowID pk.Byte
			slot     pk.Short
		)
		if windowID.Decode(r) == nil && windowID == 0 {
			if slot.Decode(r) == nil {
				var present pk.Boolean
				if present.Decode(r) == nil {
					if present {
						var item pk.VarInt
						if item.Decode(r) == nil {
							inventory[slot].Store(int32(item))
						}
					} else {
						inventory[slot].Store(0)
					}
				}
			}
		}
	case data.UpdateHealth:
		var (
			Health pk.Float
		)
		if err := p.Scan(&Health); err != nil {
			return err
		}
		if Health <= 0 {
			health.Store(uint32(Health))
			queue <- marshal(data.ClientStatus, pk.VarInt(0)) // 重生
		} else if health.Load() <= 0 {
			health.Store(uint32(Health))
			if x, z := posX.Load(), posZ.Load(); x != 0 && z != 0 && x >= -100 && x <= 100 && z >= -100 && z <= 100 {
				queue <- marshal(data.ChatMessageServerbound, pk.String("/back")) // 返回掛機點
			}
		}
	case data.PlayerPositionAndLookClientbound:
		var (
			x, y, z    pk.Double
			yaw, pitch pk.Float
			flags      pk.Byte
			TpID       pk.VarInt
		)
		if err := p.Scan(&x, &y, &z, &yaw, &pitch, &flags, &TpID); err != nil {
			return err
		}
		queue <- marshal(data.TeleportConfirm, TpID)
		if flags&0x01 == 0 {
			posX.Store(float64(x))
		} else {
			posX.Add(float64(x))
		}
		if flags&0x02 == 0 {
			posY.Store(float64(y))
		} else {
			posY.Add(float64(y))
		}
		if flags&0x04 == 0 {
			posZ.Store(float64(z))
		} else {
			posZ.Add(float64(z))
		}
	}
	return nil
}

func handleChat(msg string, queue chan<- *pk.Packet) {
	if msg == "[S] <系統> 讀取人物成功。" {
		loginSuccess = true
		queue <- marshal(data.PlayerAbilitiesServerbound, pk.Byte(0))
		return
	} else if strings.HasPrefix(msg, "[S] <領地> 沒有容器權限的領地內，你只能丟棄 : 鑽石 鑽石磚 綠寶石 綠寶石磚 鐵錠 鐵磚 金錠 金磚 地獄石英 石英磚 黑曜石 紫色界伏盒 界伏盒") {
		haveClaimPermission = false
		return
	} else if strings.HasPrefix(msg, "[廢土伺服] : ") && (strings.HasSuffix(msg, "想要你傳送到 該玩家 的位置!") || strings.HasSuffix(msg, "想要傳送到 你 的位置")) {
		playerID := strings.Split(msg[17:], " ")[0]
		if isPlayerTrust(playerID) {
			sendChat("/tok", queue)
		} else {
			sendChat("/tno", queue)
		}
		return
	} else if strings.HasPrefix(msg, "[收到私訊") {
		playerID := strings.Split(msg[14:], "]")[0]
		message := msg[18+len(playerID):]
		if playerID == "" || message == "" || !isPlayerTrust(playerID) || len(message) < 1 {
			return
		}
		if !strings.HasPrefix(message, "/") {
			return
		}
		switch strings.Split(message[1:], " ")[0] {
		case "slot":
			if len(message) >= 7 {
				slot, _ := strconv.Atoi(message[6:])
				if slot >= 1 && slot <= 9 {
					queue <- marshal(data.HeldItemChangeServerbound, pk.Short(slot-1))
					sendChat(fmt.Sprintf("/m %s [機器人] 已切換到物品欄第%v格", playerID, slot), queue)
					return
				}
			}
			sendChat("/m "+playerID+" [機器人] 指令用法:/slot <要切換的快捷欄格(1-9)>", queue)
		case "server":
			if len(message) >= 9 {
				server, _ := strconv.Atoi(message[8:])
				if server >= 1 {
					sendChat(fmt.Sprintf("/m %s [機器人] 已正常切換到第%v分流", playerID, server), queue)
					sendChat(fmt.Sprintf("/tpserver server%v", server), queue)
					return
				}
			}
			sendChat("/m "+playerID+" [機器人] 指令用法:/server <要切換的伺服器分流>", queue)
		case "fserver":
			if len(message) >= 10 {
				server, _ := strconv.Atoi(message[9:])
				if server >= 1 {
					sendChat(fmt.Sprintf("/m %s [機器人] 已強制切換到第%v分流", playerID, server), queue)
					sendChat(fmt.Sprintf("/server server%v", server), queue)
					return
				}
			}
			sendChat("/m "+playerID+" [機器人] 指令用法:/fserver <要強制切換的伺服器分流>", queue)
		case "respawn":
			queue <- marshal(data.ClientStatus, pk.VarInt(0))
			sendChat("/m "+playerID+" [機器人] 已送出重生封包", queue)
		case "back":
			sendChat("/back", queue)
			sendChat("/m "+playerID+" [機器人] 已送出/back指令", queue)
		case "throw":
			if len(message) >= 8 {
				slot, _ := strconv.Atoi(message[7:])
				if slot >= 1 && slot <= 9 {
					queue <- marshal(data.ClickWindow, pk.UnsignedByte(0), pk.Short(35+slot), pk.Byte(1), pk.Short(0), pk.VarInt(4), pk.Byte(0))
					sendChat(fmt.Sprintf("/m %s [機器人] 已丟棄快捷欄欄第%v格的物品", playerID, slot), queue)
					return
				}
			}
			sendChat("/m "+playerID+" 指令用法:/throw <要丟棄的快捷欄格(1-9)>", queue)
		case "throwi":
			if len(message) >= 9 {
				slot, _ := strconv.Atoi(message[8:])
				if slot >= 1 && slot <= 36 {
					queue <- marshal(data.ClickWindow, pk.UnsignedByte(0), pk.Short(8+slot), pk.Byte(1), pk.Short(0), pk.VarInt(4), pk.Byte(0))
					sendChat(fmt.Sprintf("/m %s [機器人] 已丟棄物品欄第%v格的物品", playerID, slot), queue)
					return
				}
			}
			sendChat("/m "+playerID+" 指令用法:/throwi <要丟棄的快捷欄格(1-36)>", queue)
		case "throwall":
			go func() {
				for i := 9; i <= 44; i++ {
					if inventory[i].Load() > 0 {
						queue <- marshal(data.ClickWindow, pk.UnsignedByte(0), pk.Short(i), pk.Byte(1), pk.Short(0), pk.VarInt(4), pk.Byte(0))
					}
				}
				sendChat("/m "+playerID+" [機器人] 已丟棄物品欄全部的物品", queue)
			}()
		case "spawn":
			sendChat("/spawn", queue)
			sendChat("/m "+playerID+" [機器人] 已送出回到重生點的指令", queue)
		case "reconnect":
			_ = c.Conn().Close()
		case "tok":
			sendChat("/tok", queue)
		case "cgm":
			sendChat("/cgm", queue)
		}
	}
}

func sendChat(msg string, queue chan<- *pk.Packet) {
	queue <- marshal(data.ChatMessageServerbound, pk.String(msg))
}

func marshal(ID int32, fields ...pk.FieldEncoder) (p *pk.Packet) {
	p = &pk.Packet{ID: ID}
	for _, v := range fields {
		p.Data = append(p.Data, v.Encode()...)
	}
	return
}

func isPlayerTrust(playerID string) bool {
	playerID = strings.ToLower(playerID)
	if len(os.Args)>=4 && playerID == strings.ToLower(os.Args[3]){
		return true
	}
	return false
}

func auth(email, password string) error {
	/*
		accounts.txt
		檔案結構: Email,UUID,ClientToken,AccountToken
	*/
	if file, err := os.Open("accounts.txt"); err == nil {
		defer func() { _ = file.Close() }()
		if line, err := ioutil.ReadAll(file); err == nil {
			if split := strings.Split(string(line), ","); len(split) == 4 && email == split[0] {
				cacheUUID := split[1]
				clToken := split[2]
				acToken := split[3]
				client := &yggdrasil.Client{ClientToken: clToken, AccessToken: acToken}
				ok, err := client.Validate()
				if err == nil && ok {
					req, err := http.NewRequest("GET", fmt.Sprintf("https://api.mojang.com/user/profiles/%v/names", cacheUUID), nil)
					if err != nil {
						return errors.New(fmt.Sprintf("[錯誤] 向Mojang查詢UUID對應的玩家ID時發生錯誤,錯誤傾印:%v", err))
					}
					req.Header.Set("User-agent", "McFalloutBot")
					req.Header.Set("Connection", "keep-alive")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return errors.New(fmt.Sprintf("[錯誤] 向Mojang查詢UUID對應的玩家ID時發生錯誤,錯誤傾印:%v", err))
					}
					if resp.StatusCode != 200 {
						return errors.New(fmt.Sprintf("[錯誤] 向Mojang查詢UUID對應的玩家ID時發生錯誤,錯誤傾印:狀態碼非200(%v)", resp.StatusCode))
					}
					body, err := ioutil.ReadAll(resp.Body)
					if err != nil {
						return errors.New(fmt.Sprintf("[錯誤] 向Mojang查詢UUID對應的玩家ID時發生錯誤,錯誤傾印:%v", err))
					}
					_ = resp.Body.Close()
					var history []struct {
						Name        string `json:"name"`
						ChangedToAt int64  `json:"changedToAt"`
					}
					if err := json.Unmarshal(body, &history); err != nil {
						return errors.New(fmt.Sprintf("[錯誤] 向Mojang查詢UUID對應的玩家ID時發生錯誤,錯誤傾印:%v", err))
					}
					c.Name = history[len(history)-1].Name
					c.Auth.UUID = cacheUUID
					c.AsTk = acToken
					return nil
				} else if err != nil && err.StatusCode != 403 {
					return errors.New(fmt.Sprintf("[錯誤] Mojang伺服器出錯,狀態碼:%v,錯誤傾印:%v", err.StatusCode, err.ErrorMessage))
				}
			}
		}
	}
	client := &yggdrasil.Client{ClientToken: uuid.New().String()}
	authResponse, err := client.Authenticate(email, password, "Minecraft", 1)
	if err != nil {
		return errors.New(fmt.Sprintf("[錯誤] 向Mojang驗證時發生錯誤,錯誤傾印:%v", err.ErrorMessage))
	}
	c.Name = authResponse.SelectedProfile.Name
	c.Auth.UUID = authResponse.SelectedProfile.ID
	c.AsTk = authResponse.AccessToken
	_ = os.Remove("accounts.txt")
	f, err2 := os.Create("accounts.txt")
	if err2 != nil {
		return errors.New(fmt.Sprintf("[錯誤] 寫入緩存至檔案時發生錯誤: %v", err2))
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(fmt.Sprintf("%v,%v,%v,%v", email, authResponse.SelectedProfile.ID, authResponse.ClientToken, authResponse.AccessToken)); err != nil {
		return errors.New(fmt.Sprintf("[錯誤] 寫入緩存至檔案時發生錯誤: %v", err))
	}
	return nil
}
