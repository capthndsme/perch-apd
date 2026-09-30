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
PERCH_VERSION:=1.1.0-pre.2
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
  The controller can also manage the AP's Wi-Fi (networks, radios,
  roaming) when the AP allows it (option wifi_config: read by default,
  write to allow changes): every change is rolled back unless the AP
  reaches the controller again and its radios and networks come up, also
  across a reboot (the perch-apd-guard boot script).
endef

define Package/perch-apd/conffiles
/etc/config/perch-apd
endef

# An apk upgrade (OpenWrt 25.x) replaces the binary without stopping the
# running daemon (opkg stops it in the old package's prerm first), and the
# "start" in default_postinst leaves a running procd service alone: without a
# restart the old version keeps running, and holds its deleted binary's flash.
# No exit here: apk inlines this after default_postinst in the same script.
# The boot guard runs before the network at every boot: enabled on upgrades
# too (default_postinst enables a package's init scripts on a first install
# only, so an upgrade from a version without the guard would leave it off).
define Package/perch-apd/postinst
#!/bin/sh
if [ -z "$${IPKG_INSTROOT}" ]; then
	/etc/init.d/perch-apd-guard enable
fi
if [ -z "$${IPKG_INSTROOT}" ] && [ "$${PKG_UPGRADE}" = "1" ] && /etc/init.d/perch-apd running; then
	/etc/init.d/perch-apd restart
fi
endef

# Removing the package (not upgrading it) drops the Wi-Fi config plane's
# state; a change still waiting for its confirm is restored first, as at a
# reboot, so the AP is never left with an unconfirmed change and no guard.
define Package/perch-apd/prerm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
[ "$${PKG_UPGRADE}" = "1" ] && exit 0
if [ -f /etc/perch-apd/plane/rollback/pending.json ]; then
	/etc/init.d/perch-apd stop 2>/dev/null
	rm -f /var/run/perch-apd/apply-*
	/usr/bin/perch-apd config-guard 2>&1 | logger -t perch-apd
	/etc/init.d/network reload
fi
exit 0
endef

define Package/perch-apd/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/perch-apd $(1)/usr/bin/perch-apd
	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/perch-apd.init $(1)/etc/init.d/perch-apd
	$(INSTALL_BIN) ./files/perch-apd-guard.init $(1)/etc/init.d/perch-apd-guard
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/perch-apd.config $(1)/etc/config/perch-apd
	$(INSTALL_DIR) $(1)/lib/upgrade/keep.d
	$(INSTALL_DATA) ./files/perch-apd.keep $(1)/lib/upgrade/keep.d/perch-apd
endef

$(eval $(call GoBinPackage,perch-apd))
$(eval $(call BuildPackage,perch-apd))
