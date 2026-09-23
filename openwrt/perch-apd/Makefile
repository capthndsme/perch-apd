#
# perch-apd: Perch AP Daemon. Replaces prometheus-node-exporter-lua-* as the
# access point's metrics source and bridges the AP to the Perch Network
# Controller over a WebSocket (kick, steer, locate, reboot).
#
# Use as a feed:  src-link perch_apd /path/to/perch-apd/openwrt
#   ./scripts/feeds update perch_apd && ./scripts/feeds install perch-apd
#   make menuconfig   # Network → Network Monitoring → perch-apd
#   make package/perch-apd/compile
# or from a checkout, with Docker: scripts/openwrt-package.sh <arch> <release>
#
# The build downloads the Go modules it needs (github.com/capthndsme/perch-agentkit
# among them) through the SDK's Go proxy settings. PKG_HASH is skip: the script
# and the release workflow place the tagged source tarball in dl/ themselves.
#
include $(TOPDIR)/rules.mk

PKG_NAME:=perch-apd
PERCH_VERSION:=1.0.0
PKG_RELEASE:=1
# PERCH_VERSION is the upstream version: it names the git tag (v1.0.0-rc.2)
# and is what --version prints. PKG_VERSION is the same version in the form
# the package manager sorts correctly: opkg (OpenWrt 24.10, .ipk) wants
# 1.0.0~rc2, which sorts below 1.0.0; apk-tools (25.12, .apk) wants 1.0.0_rc2
# and rejects both "~" and "-rc.2". scripts/openwrt-package.sh sets both
# lines explicitly for each build.
PERCH_PRE_SEP:=$(if $(CONFIG_USE_APK),_,~)
PKG_VERSION:=$(subst -alpha.,$(PERCH_PRE_SEP)alpha,$(subst -beta.,$(PERCH_PRE_SEP)beta,$(subst -pre.,$(PERCH_PRE_SEP)pre,$(subst -rc.,$(PERCH_PRE_SEP)rc,$(PERCH_VERSION)))))

PKG_SOURCE:=$(PKG_NAME)-$(PERCH_VERSION).tar.gz
PKG_SOURCE_URL:=https://codeload.github.com/capthndsme/perch-apd/tar.gz/v$(PERCH_VERSION)?
PKG_HASH:=skip
PKG_BUILD_DIR:=$(BUILD_DIR)/$(PKG_NAME)-$(PERCH_VERSION)

PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE
PKG_MAINTAINER:=capthndsme <nieoy@msn.com>

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_BUILD_FLAGS:=no-mips16

GO_PKG:=github.com/capthndsme/perch-apd
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/perch-apd
GO_PKG_LDFLAGS:=-s -w
GO_PKG_LDFLAGS_X:=$(GO_PKG)/internal/version.Version=$(PERCH_VERSION)

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/perch-apd
  SECTION:=net
  CATEGORY:=Network
  SUBMENU:=Network Monitoring
  TITLE:=Perch AP Daemon
  URL:=https://github.com/capthndsme/perch-apd
  DEPENDS:=$(GO_ARCH_DEPENDS) +ca-bundle
endef

define Package/perch-apd/description
  Access point daemon for the Perch Network Controller. Reports the AP's
  metrics (Wi-Fi interfaces and stations via nl80211, netdev, CPU, memory,
  conntrack) with prometheus-node-exporter-lua's metric names, so it
  replaces the prometheus-node-exporter-lua-* packages, and keeps a
  WebSocket session to the controller for kick, steer, locate (LED blink)
  and reboot. Joins with a token from the dashboard:
  perch-apd join --controller <url> --token <token>
endef

define Package/perch-apd/conffiles
/etc/config/perch-apd
endef

# An apk upgrade (OpenWrt 25.x) replaces the binary without stopping the
# running daemon (opkg stops it in the old package's prerm first), and the
# "start" in default_postinst leaves a running procd service alone: without a
# restart the old version keeps running, and holds its deleted binary's flash.
# No exit here: apk inlines this after default_postinst in the same script.
define Package/perch-apd/postinst
#!/bin/sh
if [ -z "$${IPKG_INSTROOT}" ] && [ "$${PKG_UPGRADE}" = "1" ] && /etc/init.d/perch-apd running; then
	/etc/init.d/perch-apd restart
fi
endef

define Package/perch-apd/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/perch-apd $(1)/usr/bin/perch-apd
	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/perch-apd.init $(1)/etc/init.d/perch-apd
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/perch-apd.config $(1)/etc/config/perch-apd
endef

$(eval $(call GoBinPackage,perch-apd))
$(eval $(call BuildPackage,perch-apd))
