# Unicode shell links for Windows PowerShell 5.1. WScript.Shell can convert
# paths through the system ANSI code page, corrupting unsupported characters.
# IShellLinkW and IPersistFile keep paths as UTF-16 when writing and reading.
# https://learn.microsoft.com/windows/win32/api/shobjidl_core/nn-shobjidl_core-ishelllinkw
if (-not ('StarPort.WindowsShortcut' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;
using System.Text;

namespace StarPort
{
    public sealed class WindowsShortcutInfo
    {
        public string TargetPath { get; private set; }
        public string WorkingDirectory { get; private set; }
        public string IconPath { get; private set; }
        public int IconIndex { get; private set; }

        internal WindowsShortcutInfo(string targetPath, string workingDirectory,
                                     string iconPath, int iconIndex)
        {
            TargetPath = targetPath;
            WorkingDirectory = workingDirectory;
            IconPath = iconPath;
            IconIndex = iconIndex;
        }
    }

    public static class WindowsShortcut
    {
        private const int PathBufferLength = 32768;
        private const uint RawPath = 0x00000004; // SLGP_RAWPATH

        [ComImport]
        [Guid("00021401-0000-0000-C000-000000000046")]
        private class ShellLink { }

        // Keep all methods in their native vtable order, including unused ones.
        // void COM methods retain HRESULT-to-exception translation by the CLR.
        [ComImport]
        [Guid("000214F9-0000-0000-C000-000000000046")]
        [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
        private interface IShellLinkW
        {
            void GetPath([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder path,
                         int maxPath, IntPtr findData, uint flags);
            void GetIDList(out IntPtr itemIdList);
            void SetIDList(IntPtr itemIdList);
            void GetDescription([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder description,
                                int maxName);
            void SetDescription([MarshalAs(UnmanagedType.LPWStr)] string description);
            void GetWorkingDirectory([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder directory,
                                     int maxPath);
            void SetWorkingDirectory([MarshalAs(UnmanagedType.LPWStr)] string directory);
            void GetArguments([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder arguments,
                              int maxPath);
            void SetArguments([MarshalAs(UnmanagedType.LPWStr)] string arguments);
            void GetHotkey(out short hotkey);
            void SetHotkey(short hotkey);
            void GetShowCmd(out int showCommand);
            void SetShowCmd(int showCommand);
            void GetIconLocation([Out, MarshalAs(UnmanagedType.LPWStr)] StringBuilder iconPath,
                                 int maxPath, out int iconIndex);
            void SetIconLocation([MarshalAs(UnmanagedType.LPWStr)] string iconPath, int iconIndex);
            void SetRelativePath([MarshalAs(UnmanagedType.LPWStr)] string path, uint reserved);
            void Resolve(IntPtr window, uint flags);
            void SetPath([MarshalAs(UnmanagedType.LPWStr)] string path);
        }

        private static string AbsolutePath(string value, string parameterName)
        {
            if (String.IsNullOrWhiteSpace(value) || !Path.IsPathRooted(value))
                throw new ArgumentException("An absolute path is required.", parameterName);
            return Path.GetFullPath(value);
        }

        public static void Create(string linkPath, string targetPath,
                                  string workingDirectory, string iconPath)
        {
            linkPath = AbsolutePath(linkPath, "linkPath");
            targetPath = AbsolutePath(targetPath, "targetPath");
            workingDirectory = AbsolutePath(workingDirectory, "workingDirectory");
            iconPath = AbsolutePath(iconPath, "iconPath");
            object instance = new ShellLink();
            try
            {
                IShellLinkW link = (IShellLinkW)instance;
                link.SetPath(targetPath);
                link.SetWorkingDirectory(workingDirectory);
                link.SetIconLocation(iconPath, 0);
                ((IPersistFile)instance).Save(linkPath, true);
            }
            finally
            {
                Marshal.FinalReleaseComObject(instance);
            }
        }

        public static WindowsShortcutInfo Read(string linkPath)
        {
            linkPath = AbsolutePath(linkPath, "linkPath");
            object instance = new ShellLink();
            try
            {
                ((IPersistFile)instance).Load(linkPath, 0); // STGM_READ
                IShellLinkW link = (IShellLinkW)instance;
                StringBuilder target = new StringBuilder(PathBufferLength);
                StringBuilder directory = new StringBuilder(PathBufferLength);
                StringBuilder icon = new StringBuilder(PathBufferLength);
                int iconIndex;
                // Reading must not Resolve, search for, or launch the target.
                link.GetPath(target, target.Capacity, IntPtr.Zero, RawPath);
                link.GetWorkingDirectory(directory, directory.Capacity);
                link.GetIconLocation(icon, icon.Capacity, out iconIndex);
                return new WindowsShortcutInfo(target.ToString(), directory.ToString(),
                                               icon.ToString(), iconIndex);
            }
            finally
            {
                Marshal.FinalReleaseComObject(instance);
            }
        }
    }
}
'@
}
