# rawBeacon

**rawBeacon** is a lightweight service discovery and tag distribution tool for local networks.  
It allows multiple computers (or small devices such as M5Stick) on the same LAN to automatically discover each other, exchange a unique **Beacon ID**, and share custom **tags** that describe their role or capabilities.  

The project is implemented in **Go** and provides a simple **Fyne GUI** for control.

---

## Features

- 🌐 **Service Discovery**  
  Beacons automatically discover each other via UDP broadcast and loopback.  

- 🏷️ **Tag Management**  
  Each beacon can define arbitrary tags (e.g., *IsServer*, *IsClient*, *RenderNode*).  
  Tags are distributed across the network and can be queried by other tools or the included **Consumer**.

- 🖥️ **Fyne GUI**  
  Start/stop the beacon, configure ports, view discovered peers, and manage tags through a simple cross-platform UI.

- 🔄 **Sidecar Concept**  
  Unity applications or external tools don’t need to run their own discovery — they can ask the local beacon for known peers and their tags.

---

## How It Works

1. Each beacon periodically broadcasts its **ID + Name + Listen Port** on the LAN.  
2. Other beacons receive these broadcasts and maintain a peer list (with last-seen timestamps).  
3. Tags can be created/removed via the GUI.  
4. Tags are distributed on demand:  
   - A **consumer** (or another beacon) can send a `/beacon/tags/request`.  
   - The target beacon responds with its tag list.
