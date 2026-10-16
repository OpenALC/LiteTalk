; 轻音 LiteTalk Windows 安装器（自包含 NSIS 脚本，替代 v2 wails CLI 生成的 project.nsi）
; 用法（CI）：
;   makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\LiteTalk.exe -DINFO_PRODUCTVERSION=1.2.3 project.nsi
; 产物：..\..\bin\LiteTalk-amd64-installer.exe
Unicode true

!define INFO_PROJECTNAME    "LiteTalk"
!define INFO_COMPANYNAME    "OpenALC"
!define INFO_PRODUCTNAME    "轻音 LiteTalk"
; CI 从标签注入（三段纯数字）；本地手动打包未传时回落 0.0.0
!ifndef INFO_PRODUCTVERSION
  !define INFO_PRODUCTVERSION "0.0.0"
!endif
!define INFO_COPYRIGHT      "(c) OpenALC"
!define PRODUCT_EXECUTABLE  "LiteTalk.exe"

; 版本信息须为 4 段（补丁位固定 0，正式版本由 INFO_PRODUCTVERSION 决定）
VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"
VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} 安装程序"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

ManifestDPIAware true

!include "MUI.nsh"
!include "FileFunc.nsh"
!include "x64.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
!define MUI_FINISHPAGE_NOAUTOCLOSE
!define MUI_ABORTWARNING

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\bin\${INFO_PROJECTNAME}-amd64-installer.exe"
InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
ShowInstDetails show
RequestExecutionLevel admin

; 待安装二进制由 makensis -DARG_WAILS_AMD64_BINARY=... 注入（缺省为构建产物）
!ifndef ARG_WAILS_AMD64_BINARY
  !define ARG_WAILS_AMD64_BINARY "..\..\bin\LiteTalk.exe"
!endif

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "轻音 LiteTalk 仅支持 64 位 Windows。"
    Abort
  ${EndIf}
  SetRegView 64
FunctionEnd

Section
    SetShellVarContext all
    SetOutPath $INSTDIR

    File /oname=${PRODUCT_EXECUTABLE} "${ARG_WAILS_AMD64_BINARY}"
    File /oname=icon.ico "..\icon.ico"

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    WriteUninstaller "$INSTDIR\uninstall.exe"

    ; 注册表：卸载入口（控制面板「应用和功能」可见）
    WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${INFO_COMPANYNAME}${INFO_PROJECTNAME}" \
        "DisplayName" "${INFO_PRODUCTNAME}"
    WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${INFO_COMPANYNAME}${INFO_PROJECTNAME}" \
        "DisplayIcon" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${INFO_COMPANYNAME}${INFO_PROJECTNAME}" \
        "Publisher" "${INFO_COMPANYNAME}"
    WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${INFO_COMPANYNAME}${INFO_PROJECTNAME}" \
        "DisplayVersion" "${INFO_PRODUCTVERSION}"
    WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${INFO_COMPANYNAME}${INFO_PROJECTNAME}" \
        "UninstallString" "$INSTDIR\uninstall.exe"
SectionEnd

Section "uninstall"
    SetShellVarContext all

    ; WebView2 用户数据目录（固定数据目录名，与 wails 默认一致）
    RMDir /r "$AppData\${INFO_COMPANYNAME}\${INFO_PROJECTNAME}"
    RMDir /r "$LocalAppData\${INFO_COMPANYNAME}\${INFO_PROJECTNAME}"

    RMDir /r $INSTDIR

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${INFO_COMPANYNAME}${INFO_PROJECTNAME}"
SectionEnd
