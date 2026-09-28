// Explorer context menu for squashoverlay.
//
// On a .sqfs/.squashfs/.wsquashfs file it adds "Mount as" and "Mount read-only
// as" submenus listing the free drive letters, or "Open X:" and "Unmount X:"
// when the image is already mounted. On a squashoverlay drive it adds
// "Unmount". Mounting runs "mount.exe shell-mount" (next to this DLL) with no
// console; unmounting signals the event the mount process waits on.
//
// Registered by scripts/install-shell.ps1: under HKCU when UAC is on, under
// HKLM when it is off (Explorer then ignores per-user shell extensions).

#include <windows.h>
#include <shlobj.h>
#include <shellapi.h>

#include <new>
#include <string>
#include <vector>

// {0ED6EEBA-7DB7-4DE4-B8B0-1D355E1EE80D}
static const CLSID CLSID_SquashoverlayMenu = {
    0x0ed6eeba, 0x7db7, 0x4de4, {0xb8, 0xb0, 0x1d, 0x35, 0x5e, 0x1e, 0xe8, 0x0d}};

static HMODULE g_module;
static LONG g_objects;

namespace {

const wchar_t* const kImageExtensions[] = {L".sqfs", L".squashfs", L".wsquashfs"};
const wchar_t kFsName[] = L"squashoverlay";

std::wstring ModuleDir() {
    wchar_t buf[MAX_PATH];
    DWORD n = GetModuleFileNameW(g_module, buf, MAX_PATH);
    std::wstring p(buf, n);
    return p.substr(0, p.find_last_of(L'\\'));
}

std::wstring StateDir() {
    PWSTR local = nullptr;
    std::wstring dir;
    if (SUCCEEDED(SHGetKnownFolderPath(FOLDERID_LocalAppData, 0, nullptr, &local))) {
        dir = std::wstring(local) + L"\\squashoverlay";
    }
    CoTaskMemFree(local);
    return dir;
}

bool EndsWithNoCase(const std::wstring& s, const wchar_t* suffix) {
    size_t n = wcslen(suffix);
    return s.size() >= n && _wcsicmp(s.c_str() + s.size() - n, suffix) == 0;
}

bool IsImagePath(const std::wstring& p) {
    for (auto ext : kImageExtensions) {
        if (EndsWithNoCase(p, ext)) return true;
    }
    return false;
}

std::wstring DriveRoot(wchar_t letter) { return std::wstring{letter, L':', L'\\'}; }
std::wstring DriveName(wchar_t letter) { return std::wstring{letter, L':'}; }

// Free letters D: to Z:, in order.
std::vector<wchar_t> FreeLetters() {
    DWORD used = GetLogicalDrives();
    std::vector<wchar_t> free;
    for (wchar_t c = L'D'; c <= L'Z'; ++c) {
        if (!(used & (1u << (c - L'A')))) free.push_back(c);
    }
    return free;
}

// Whether the drive is served by squashoverlay. Network and removable drives
// are skipped so the check never waits on a disconnected share.
bool IsSquashoverlayDrive(wchar_t letter) {
    std::wstring root = DriveRoot(letter);
    UINT type = GetDriveTypeW(root.c_str());
    if (type != DRIVE_FIXED && type != DRIVE_RAMDISK) return false;
    wchar_t fs[MAX_PATH + 1] = {};
    if (!GetVolumeInformationW(root.c_str(), nullptr, 0, nullptr, nullptr, nullptr, fs, MAX_PATH)) {
        return false;
    }
    return _wcsicmp(fs, kFsName) == 0;
}

bool ProcessAlive(DWORD pid) {
    HANDLE h = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid);
    if (!h) return false;
    DWORD code = 0;
    bool alive = GetExitCodeProcess(h, &code) && code == STILL_ACTIVE;
    CloseHandle(h);
    return alive;
}

// Reads a "key=value" state file written by mount.exe (UTF-8).
bool ReadState(const std::wstring& path, std::wstring* image, DWORD* pid) {
    HANDLE f = CreateFileW(path.c_str(), GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                           nullptr, OPEN_EXISTING, 0, nullptr);
    if (f == INVALID_HANDLE_VALUE) return false;
    char buf[8192];
    DWORD n = 0;
    BOOL ok = ReadFile(f, buf, sizeof(buf) - 1, &n, nullptr);
    CloseHandle(f);
    if (!ok) return false;
    buf[n] = 0;

    std::string text(buf, n), img8;
    *pid = 0;
    size_t pos = 0;
    while (pos < text.size()) {
        size_t end = text.find('\n', pos);
        if (end == std::string::npos) end = text.size();
        std::string line = text.substr(pos, end - pos);
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.rfind("image=", 0) == 0) img8 = line.substr(6);
        if (line.rfind("pid=", 0) == 0) *pid = strtoul(line.c_str() + 4, nullptr, 10);
        pos = end + 1;
    }
    int wn = MultiByteToWideChar(CP_UTF8, 0, img8.data(), (int)img8.size(), nullptr, 0);
    image->assign(wn, L'\0');
    MultiByteToWideChar(CP_UTF8, 0, img8.data(), (int)img8.size(), image->data(), wn);
    return *pid != 0;
}

// The drive letter where image is mounted by a live mount.exe, or 0.
wchar_t MountedDriveFor(const std::wstring& image) {
    std::wstring dir = StateDir() + L"\\mounts\\";
    WIN32_FIND_DATAW fd;
    HANDLE h = FindFirstFileW((dir + L"*.txt").c_str(), &fd);
    if (h == INVALID_HANDLE_VALUE) return 0;
    wchar_t found = 0;
    do {
        std::wstring stateImage;
        DWORD pid = 0;
        if (wcslen(fd.cFileName) == 5 && ReadState(dir + fd.cFileName, &stateImage, &pid) &&
            ProcessAlive(pid) && _wcsicmp(stateImage.c_str(), image.c_str()) == 0) {
            found = fd.cFileName[0];
        }
    } while (!found && FindNextFileW(h, &fd));
    FindClose(h);
    return found;
}

void ShowError(HWND owner, const std::wstring& msg) {
    MessageBoxW(owner, msg.c_str(), L"squashoverlay", MB_OK | MB_ICONERROR);
}

struct Action {
    enum Type { Mount, MountReadOnly, Open, Unmount } type;
    wchar_t letter;
};

class ContextMenu : public IShellExtInit, public IContextMenu {
public:
    ContextMenu() { InterlockedIncrement(&g_objects); }

    // IUnknown
    IFACEMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IShellExtInit) {
            *ppv = static_cast<IShellExtInit*>(this);
        } else if (riid == IID_IContextMenu) {
            *ppv = static_cast<IContextMenu*>(this);
        } else {
            *ppv = nullptr;
            return E_NOINTERFACE;
        }
        AddRef();
        return S_OK;
    }
    IFACEMETHODIMP_(ULONG) AddRef() override { return InterlockedIncrement(&refs_); }
    IFACEMETHODIMP_(ULONG) Release() override {
        ULONG n = InterlockedDecrement(&refs_);
        if (n == 0) delete this;
        return n;
    }

    // IShellExtInit
    IFACEMETHODIMP Initialize(PCIDLIST_ABSOLUTE, IDataObject* data, HKEY) override {
        kind_ = Kind::None;
        if (!data) return E_INVALIDARG;
        FORMATETC fe = {CF_HDROP, nullptr, DVASPECT_CONTENT, -1, TYMED_HGLOBAL};
        STGMEDIUM sm = {};
        if (FAILED(data->GetData(&fe, &sm))) return E_FAIL;
        if (auto drop = static_cast<HDROP>(GlobalLock(sm.hGlobal))) {
            if (DragQueryFileW(drop, 0xFFFFFFFF, nullptr, 0) == 1) {
                UINT len = DragQueryFileW(drop, 0, nullptr, 0);
                path_.assign(len, L'\0');
                DragQueryFileW(drop, 0, path_.data(), len + 1);
            }
            GlobalUnlock(sm.hGlobal);
        }
        ReleaseStgMedium(&sm);

        if (path_.size() == 3 && path_[1] == L':' && path_[2] == L'\\') {
            letter_ = static_cast<wchar_t>(towupper(path_[0]));
            if (IsSquashoverlayDrive(letter_)) kind_ = Kind::Drive;
        } else if (IsImagePath(path_)) {
            kind_ = Kind::Image;
        }
        return kind_ == Kind::None ? E_FAIL : S_OK;
    }

    // IContextMenu
    IFACEMETHODIMP QueryContextMenu(HMENU menu, UINT index, UINT first, UINT last, UINT flags) override {
        actions_.clear();
        if (flags & CMF_DEFAULTONLY) return MAKE_HRESULT(SEVERITY_SUCCESS, 0, 0);
        first_ = first;
        last_ = last;

        if (kind_ == Kind::Drive) {
            Add(menu, index++, L"Unmount", {Action::Unmount, letter_});
        } else if (kind_ == Kind::Image) {
            if (wchar_t d = MountedDriveFor(path_)) {
                Add(menu, index++, L"Open " + DriveName(d), {Action::Open, d});
                Add(menu, index++, L"Unmount " + DriveName(d), {Action::Unmount, d});
            } else {
                InsertSubmenu(menu, index++, L"Mount as", Action::Mount);
                InsertSubmenu(menu, index++, L"Mount read-only as", Action::MountReadOnly);
            }
        }
        return MAKE_HRESULT(SEVERITY_SUCCESS, 0, static_cast<USHORT>(actions_.size()));
    }

    IFACEMETHODIMP InvokeCommand(LPCMINVOKECOMMANDINFO ici) override {
        if (!IS_INTRESOURCE(ici->lpVerb)) return E_FAIL;  // no string verbs
        UINT i = LOWORD(reinterpret_cast<UINT_PTR>(ici->lpVerb));
        if (i >= actions_.size()) return E_FAIL;
        const Action& a = actions_[i];
        switch (a.type) {
        case Action::Mount:
        case Action::MountReadOnly:
            return RunMount(ici->hwnd, a.letter, a.type == Action::MountReadOnly);
        case Action::Open:
            ShellExecuteW(ici->hwnd, L"open", DriveRoot(a.letter).c_str(), nullptr, nullptr, SW_SHOWNORMAL);
            return S_OK;
        case Action::Unmount:
            return RunUnmount(ici->hwnd, a.letter);
        }
        return E_FAIL;
    }

    IFACEMETHODIMP GetCommandString(UINT_PTR, UINT, UINT*, CHAR*, UINT) override { return E_NOTIMPL; }

private:
    enum class Kind { None, Image, Drive };

    ~ContextMenu() { InterlockedDecrement(&g_objects); }

    bool Add(HMENU menu, UINT pos, const std::wstring& text, Action a) {
        UINT id = first_ + static_cast<UINT>(actions_.size());
        if (id > last_) return false;
        InsertMenuW(menu, pos, MF_BYPOSITION | MF_STRING, id, text.c_str());
        actions_.push_back(a);
        return true;
    }

    void InsertSubmenu(HMENU menu, UINT pos, const wchar_t* text, Action::Type type) {
        HMENU sub = CreatePopupMenu();
        std::vector<wchar_t> free = FreeLetters();
        if (free.empty()) {
            AppendMenuW(sub, MF_STRING | MF_GRAYED, 0, L"No free drive letters");
        } else {
            wchar_t next = free.back();  // highest free letter, away from USB drives
            UINT n = GetMenuItemCount(sub);
            Add(sub, n, L"Next free letter (" + DriveName(next) + L")", {type, next});
            AppendMenuW(sub, MF_SEPARATOR, 0, nullptr);
            for (wchar_t c : free) Add(sub, GetMenuItemCount(sub), DriveName(c), {type, c});
        }
        MENUITEMINFOW mii = {sizeof(mii)};
        mii.fMask = MIIM_SUBMENU | MIIM_STRING;
        mii.hSubMenu = sub;
        mii.dwTypeData = const_cast<wchar_t*>(text);
        InsertMenuItemW(menu, pos, TRUE, &mii);
    }

    HRESULT RunMount(HWND owner, wchar_t letter, bool readOnly) {
        std::wstring exe = ModuleDir() + L"\\mount.exe";
        std::wstring cmd = L"\"" + exe + L"\" shell-mount -drive " + DriveName(letter) +
                           (readOnly ? L" -ro" : L"") + L" \"" + path_ + L"\"";
        STARTUPINFOW si = {sizeof(si)};
        PROCESS_INFORMATION pi = {};
        if (!CreateProcessW(exe.c_str(), cmd.data(), nullptr, nullptr, FALSE, CREATE_NO_WINDOW, nullptr,
                            nullptr, &si, &pi)) {
            ShowError(owner, L"Could not start " + exe);
            return HRESULT_FROM_WIN32(GetLastError());
        }
        CloseHandle(pi.hThread);
        CloseHandle(pi.hProcess);
        return S_OK;
    }

    HRESULT RunUnmount(HWND owner, wchar_t letter) {
        std::wstring name = L"Local\\squashoverlay-unmount-" + std::wstring(1, letter);
        HANDLE ev = OpenEventW(EVENT_MODIFY_STATE, FALSE, name.c_str());
        if (!ev) {
            ShowError(owner, DriveName(letter) + L" was not mounted by squashoverlay in this session.");
            return E_FAIL;
        }
        SetEvent(ev);
        CloseHandle(ev);
        return S_OK;
    }

    LONG refs_ = 1;
    Kind kind_ = Kind::None;
    std::wstring path_;
    wchar_t letter_ = 0;
    std::vector<Action> actions_;
    UINT first_ = 0, last_ = 0;
};

class ClassFactory : public IClassFactory {
public:
    IFACEMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IClassFactory) {
            *ppv = static_cast<IClassFactory*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    IFACEMETHODIMP_(ULONG) AddRef() override { return 2; }  // static instance
    IFACEMETHODIMP_(ULONG) Release() override { return 1; }
    IFACEMETHODIMP CreateInstance(IUnknown* outer, REFIID riid, void** ppv) override {
        if (outer) return CLASS_E_NOAGGREGATION;
        auto* m = new (std::nothrow) ContextMenu();
        if (!m) return E_OUTOFMEMORY;
        HRESULT hr = m->QueryInterface(riid, ppv);
        m->Release();
        return hr;
    }
    IFACEMETHODIMP LockServer(BOOL lock) override {
        lock ? InterlockedIncrement(&g_objects) : InterlockedDecrement(&g_objects);
        return S_OK;
    }
};

ClassFactory g_factory;

}  // namespace

extern "C" BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        g_module = instance;
        DisableThreadLibraryCalls(instance);
    }
    return TRUE;
}

STDAPI DllGetClassObject(REFCLSID clsid, REFIID riid, void** ppv) {
    if (clsid != CLSID_SquashoverlayMenu) return CLASS_E_CLASSNOTAVAILABLE;
    return g_factory.QueryInterface(riid, ppv);
}

STDAPI DllCanUnloadNow() { return g_objects == 0 ? S_OK : S_FALSE; }
