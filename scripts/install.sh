#!/bin/bash
# ==============================================================================
# StreamSwitch - VPS Installation Script
# For Oracle Cloud Free Tier (Ubuntu/Oracle Linux - ARM64 or AMD64)
# ==============================================================================

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${CYAN}╔═══════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║   StreamSwitch - VPS Installer                 ║${NC}"
echo -e "${CYAN}╚═══════════════════════════════════════════════╝${NC}"
echo ""

# Detect OS
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS=$ID
    echo -e "OS: ${GREEN}$PRETTY_NAME${NC}"
else
    echo -e "${RED}Cannot detect OS${NC}"
    exit 1
fi

# Detect architecture
ARCH=$(uname -m)
echo -e "Arch: ${GREEN}$ARCH${NC}"
echo ""

# ============================================================================
# 1. Install FFmpeg (7.0+ for Enhanced RTMP H.265 support)
# ============================================================================
echo -e "${YELLOW}[1/4] Installing FFmpeg...${NC}"

install_ffmpeg_ubuntu() {
    sudo apt-get update -qq
    sudo apt-get install -y -qq ffmpeg

    # Check version
    FFMPEG_VERSION=$(ffmpeg -version 2>/dev/null | head -1 | grep -oP 'ffmpeg version \K[0-9]+\.[0-9]+' || echo "0.0")
    MAJOR=$(echo "$FFMPEG_VERSION" | cut -d. -f1)

    if [ "$MAJOR" -lt 7 ]; then
        echo -e "${YELLOW}FFmpeg version $FFMPEG_VERSION detected. For H.265 Enhanced RTMP, version 7.0+ is recommended.${NC}"
        echo -e "${YELLOW}Installing FFmpeg from static build...${NC}"
        install_ffmpeg_static
    else
        echo -e "${GREEN}FFmpeg $FFMPEG_VERSION installed${NC}"
    fi
}

install_ffmpeg_oracle_linux() {
    sudo dnf install -y epel-release || true
    sudo dnf install -y ffmpeg || install_ffmpeg_static
}

install_ffmpeg_static() {
    echo -e "${YELLOW}Downloading FFmpeg static build...${NC}"
    case $ARCH in
        x86_64|amd64)
            FFMPEG_URL="https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz"
            ;;
        aarch64|arm64)
            FFMPEG_URL="https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-arm64-static.tar.xz"
            ;;
        *)
            echo -e "${RED}Unsupported architecture: $ARCH${NC}"
            exit 1
            ;;
    esac

    cd /tmp
    wget -q "$FFMPEG_URL" -O ffmpeg-static.tar.xz
    tar xf ffmpeg-static.tar.xz
    sudo cp ffmpeg-*-static/ffmpeg /usr/local/bin/
    sudo cp ffmpeg-*-static/ffprobe /usr/local/bin/
    rm -rf ffmpeg-static.tar.xz ffmpeg-*-static
    echo -e "${GREEN}FFmpeg static build installed to /usr/local/bin/${NC}"
}

case $OS in
    ubuntu|debian)
        install_ffmpeg_ubuntu
        ;;
    ol|oraclelinux|rhel|centos|rocky|alma)
        install_ffmpeg_oracle_linux
        ;;
    *)
        echo -e "${YELLOW}Unknown OS, attempting static FFmpeg install...${NC}"
        install_ffmpeg_static
        ;;
esac

# ============================================================================
# 2. Install Go (for building from source)
# ============================================================================
echo ""
echo -e "${YELLOW}[2/4] Installing Go...${NC}"

if command -v go &> /dev/null; then
    GO_VERSION=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+')
    echo -e "${GREEN}Go $GO_VERSION already installed${NC}"
else
    case $ARCH in
        x86_64|amd64)
            GO_ARCH="amd64"
            ;;
        aarch64|arm64)
            GO_ARCH="arm64"
            ;;
        *)
            echo -e "${RED}Unsupported architecture: $ARCH${NC}"
            exit 1
            ;;
    esac

    GO_VERSION="1.22.4"
    wget -q "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -O /tmp/go.tar.gz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tar.gz
    rm /tmp/go.tar.gz

    echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh > /dev/null
    export PATH=$PATH:/usr/local/go/bin

    echo -e "${GREEN}Go $GO_VERSION installed${NC}"
fi

# ============================================================================
# 3. Build StreamSwitch
# ============================================================================
echo ""
echo -e "${YELLOW}[3/4] Building StreamSwitch...${NC}"

INSTALL_DIR="/opt/streamswitch"
sudo mkdir -p "$INSTALL_DIR"

# Copy source files
SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
sudo cp -r "$SCRIPT_DIR"/* "$INSTALL_DIR/" 2>/dev/null || true

cd "$INSTALL_DIR"

# Download dependencies and build
sudo /usr/local/go/bin/go mod tidy 2>/dev/null || go mod tidy
sudo /usr/local/go/bin/go build -o streamswitch . 2>/dev/null || go build -o streamswitch .

echo -e "${GREEN}StreamSwitch built successfully${NC}"

# ============================================================================
# 4. Create systemd service
# ============================================================================
echo ""
echo -e "${YELLOW}[4/4] Setting up systemd service...${NC}"

# Create data directory
sudo mkdir -p /opt/streamswitch/data

# Copy service file
sudo cp "$SCRIPT_DIR/scripts/streamswitch.service" /etc/systemd/system/ 2>/dev/null || \
sudo cp "$INSTALL_DIR/scripts/streamswitch.service" /etc/systemd/system/ 2>/dev/null || true

sudo systemctl daemon-reload
sudo systemctl enable streamswitch

echo -e "${GREEN}Systemd service installed${NC}"

# ============================================================================
# Open firewall port
# ============================================================================
echo ""
echo -e "${YELLOW}Opening firewall ports...${NC}"

# iptables (Oracle Cloud uses this)
sudo iptables -I INPUT -p tcp --dport 80 -j ACCEPT 2>/dev/null || true
sudo iptables -I INPUT -p udp --dport 8282 -j ACCEPT 2>/dev/null || true

# Save iptables rules
sudo netfilter-persistent save 2>/dev/null || \
sudo iptables-save | sudo tee /etc/iptables/rules.v4 > /dev/null 2>/dev/null || true

# firewalld (if present)
sudo firewall-cmd --permanent --add-port=80/tcp 2>/dev/null || true
sudo firewall-cmd --permanent --add-port=8282/udp 2>/dev/null || true
sudo firewall-cmd --reload 2>/dev/null || true

echo -e "${GREEN}Firewall configured${NC}"

# ============================================================================
# Done
# ============================================================================
echo ""
echo -e "${GREEN}╔═══════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║   Installation Complete!                       ║${NC}"
echo -e "${GREEN}╚═══════════════════════════════════════════════╝${NC}"
echo ""
echo -e "Next steps:"
echo -e "  1. Create a fallback file:"
echo -e "     ${CYAN}cd $INSTALL_DIR && bash scripts/create_fallback.sh your_image.png${NC}"
echo ""
echo -e "  2. Edit the service config if needed:"
echo -e "     ${CYAN}sudo nano /etc/systemd/system/streamswitch.service${NC}"
echo ""
echo -e "  3. Start the service:"
echo -e "     ${CYAN}sudo systemctl start streamswitch${NC}"
echo ""
echo -e "  4. Check status:"
echo -e "     ${CYAN}sudo systemctl status streamswitch${NC}"
echo ""
echo -e "  5. Open Web UI at: ${CYAN}http://YOUR_VPS_IP${NC}"
echo ""
echo -e "  6. View logs:"
echo -e "     ${CYAN}sudo journalctl -u streamswitch -f${NC}"
